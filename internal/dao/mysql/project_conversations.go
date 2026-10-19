package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

func createProjectConversationTx(ctx context.Context, tx *sql.Tx, projectID uint64, title string, current json.RawMessage) (domain.Conversation, error) {
	var next uint32
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(conversation_no),0)+1 FROM conversations WHERE project_id=?`, projectID).Scan(&next); err != nil {
		return domain.Conversation{}, fmt.Errorf("next project conversation number: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversations(project_id,conversation_no,title,current_context) VALUES(?,?,?,?)`, projectID, next, title, nullableJSON(current))
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("insert project conversation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Conversation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET active_conversation_id=? WHERE id=?`, id, projectID); err != nil {
		return domain.Conversation{}, fmt.Errorf("activate project conversation: %w", err)
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE id=?`, id))
	if err == nil {
		conversation.Selected = true
	}
	return conversation, err
}

func (s *Store) ListProjectConversations(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, status string, limit int, cursor *domain.PagePosition) ([]domain.Conversation, *domain.PagePosition, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return nil, nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if _, err = s.getProject(ctx, owner, projectNo); err != nil {
		return nil, nil, err
	}
	query := `SELECT ` + prefixConversationColumns("c") + ` FROM projects p JOIN conversations c ON c.project_id=p.id WHERE p.` + column + `=? AND p.project_no=? AND c.status=?`
	args := []any{owner.ID, projectNo, status}
	if cursor != nil {
		query += ` AND (c.updated_at<? OR (c.updated_at=? AND c.id<?))`
		args = append(args, cursor.Time, cursor.Time, cursor.ID)
	}
	query += ` ORDER BY c.updated_at DESC,c.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list project conversations: %w", err)
	}
	defer rows.Close()
	items, err := scanConversations(rows)
	if err != nil {
		return nil, nil, err
	}
	var next *domain.PagePosition
	if len(items) > limit {
		last := items[limit-1]
		next = &domain.PagePosition{Time: last.UpdatedAt, ID: last.ID}
		items = items[:limit]
	}
	return items, next, nil
}

func (s *Store) CreateProjectConversation(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, title string, current json.RawMessage) (domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Conversation{}, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Conversation{}, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Conversation{}, dao.ErrProjectArchived
	}
	conversation, err := createProjectConversationTx(ctx, tx, project.ID, title, current)
	if err != nil {
		return domain.Conversation{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, err
	}
	return conversation, nil
}

func (s *Store) GetProjectConversation(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32) (domain.Conversation, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return domain.Conversation{}, err
	}
	if _, err = s.getProject(ctx, owner, projectNo); err != nil {
		return domain.Conversation{}, err
	}
	conversation, err := scanConversation(s.db.QueryRowContext(ctx, `SELECT `+prefixConversationColumns("c")+` FROM projects p JOIN conversations c ON c.project_id=p.id WHERE p.`+column+`=? AND p.project_no=? AND c.conversation_no=?`, owner.ID, projectNo, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Conversation{}, dao.ErrConversationNotFound
	}
	return conversation, err
}

func (s *Store) RenameProjectConversation(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32, title string) (domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Conversation{}, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Conversation{}, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Conversation{}, dao.ErrProjectArchived
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Conversation{}, dao.ErrConversationNotFound
	}
	if err != nil {
		return domain.Conversation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET title=? WHERE id=?`, strings.TrimSpace(title), conversation.ID); err != nil {
		return domain.Conversation{}, err
	}
	conversation.Title = strings.TrimSpace(title)
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, err
	}
	return conversation, nil
}

func (s *Store) SelectProjectConversation(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32) (domain.Project, domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Project{}, domain.Conversation{}, dao.ErrProjectArchived
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Project{}, domain.Conversation{}, dao.ErrConversationNotFound
	}
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	if conversation.Status != domain.ConversationStatusActive {
		return domain.Project{}, domain.Conversation{}, dao.ErrConversationArchived
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET active_conversation_id=? WHERE id=?`, conversation.ID, project.ID); err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	if err = setOwnerActiveProject(ctx, tx, owner, &project.ID); err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	project.Selected = true
	project.ActiveConversationID = &conversation.ID
	project.ActiveConversationNo = &conversation.ConversationNo
	conversation.Selected = true
	if err = tx.Commit(); err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	return project, conversation, nil
}

func (s *Store) ArchiveProjectConversation(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32) (domain.Conversation, *domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Conversation{}, nil, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Conversation{}, nil, dao.ErrProjectArchived
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Conversation{}, nil, dao.ErrConversationNotFound
	}
	if err != nil {
		return domain.Conversation{}, nil, err
	}
	if conversation.Status != domain.ConversationStatusActive {
		return domain.Conversation{}, nil, dao.ErrConversationArchived
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET status='archived' WHERE id=?`, conversation.ID); err != nil {
		return domain.Conversation{}, nil, err
	}
	conversation.Status = domain.ConversationStatusArchived
	conversation.Selected = false
	var fallback *domain.Conversation
	if project.ActiveConversationID != nil && *project.ActiveConversationID == conversation.ID {
		value, e := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND status='active' ORDER BY updated_at DESC,id DESC LIMIT 1 FOR UPDATE`, project.ID))
		if e != nil && !errors.Is(e, dao.ErrNotFound) {
			return domain.Conversation{}, nil, e
		}
		var id *uint64
		if e == nil {
			value.Selected = true
			fallback = &value
			id = &value.ID
		}
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET active_conversation_id=? WHERE id=?`, nullableUint64(id), project.ID); err != nil {
			return domain.Conversation{}, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, nil, err
	}
	return conversation, fallback, nil
}

func (s *Store) RestoreProjectConversation(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32) (domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Conversation{}, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Conversation{}, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Conversation{}, dao.ErrProjectArchived
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Conversation{}, dao.ErrConversationNotFound
	}
	if err != nil {
		return domain.Conversation{}, err
	}
	if conversation.Status == domain.ConversationStatusArchived {
		if _, err = tx.ExecContext(ctx, `UPDATE conversations SET status='active' WHERE id=?`, conversation.ID); err != nil {
			return domain.Conversation{}, err
		}
		conversation.Status = domain.ConversationStatusActive
	}
	if project.Status == domain.ProjectStatusActive && project.ActiveConversationID == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET active_conversation_id=? WHERE id=?`, conversation.ID, project.ID); err != nil {
			return domain.Conversation{}, err
		}
		conversation.Selected = true
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, err
	}
	return conversation, nil
}

func (s *Store) InsertProjectMessage(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32, message domain.Message) (domain.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Message{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	activeID, err := lockProjectOwner(ctx, tx, owner)
	if err != nil {
		return domain.Message{}, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Message{}, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Message{}, dao.ErrProjectArchived
	}
	if activeID == nil || *activeID != project.ID {
		return domain.Message{}, dao.ErrResourceStateConflict
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Message{}, dao.ErrConversationNotFound
	}
	if err != nil {
		return domain.Message{}, err
	}
	if conversation.Status != domain.ConversationStatusActive {
		return domain.Message{}, dao.ErrConversationArchived
	}
	if project.ActiveConversationID == nil || *project.ActiveConversationID != conversation.ID {
		return domain.Message{}, dao.ErrResourceStateConflict
	}
	status := message.DeliveryStatus
	if status == "" {
		if message.Role == "user" {
			status = "received"
		} else {
			status = "reserved"
		}
	}
	seq := uint32(0)
	if message.MessageSeq != nil {
		seq = *message.MessageSeq
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(conversation_id,role,qq_msg_id,qq_outbound_id,msg_seq,content,shown_papers,agent_trace,delivery_status,cost_usd) VALUES(?,?,?,?,?,?,?,?,?,?)`, conversation.ID, message.Role, nullableString(message.QQMessageID), nullableString(message.QQOutboundID), seq, nullableString(message.Content), nullableJSON(message.ShownPapers), nullableJSON(message.AgentTrace), status, message.CostUSD)
	if err != nil {
		return domain.Message{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Message{}, err
	}
	message.ID = uint64(id)
	message.ConversationID = conversation.ID
	message.DeliveryStatus = status
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at=CURRENT_TIMESTAMP(3) WHERE id=?`, conversation.ID); err != nil {
		return domain.Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Message{}, err
	}
	return message, nil
}

func (s *Store) SaveProjectSearchResult(ctx context.Context, owner domain.ProjectOwner, projectNo, conversationNo uint32, current json.RawMessage, message domain.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	activeID, err := lockProjectOwner(ctx, tx, owner)
	if err != nil {
		return err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return err
	}
	if project.Status != domain.ProjectStatusActive {
		return dao.ErrProjectArchived
	}
	if activeID == nil || *activeID != project.ID {
		return dao.ErrResourceStateConflict
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, conversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return dao.ErrConversationNotFound
	}
	if err != nil {
		return err
	}
	if conversation.Status != domain.ConversationStatusActive {
		return dao.ErrConversationArchived
	}
	if project.ActiveConversationID == nil || *project.ActiveConversationID != conversation.ID {
		return dao.ErrResourceStateConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET current_context=?,updated_at=CURRENT_TIMESTAMP(3) WHERE id=?`, nullableJSON(current), conversation.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(conversation_id,role,content,shown_papers,delivery_status,cost_usd) VALUES(?,'bot',?,?,'sent',?)`, conversation.ID, nullableString(message.Content), nullableJSON(message.ShownPapers), message.CostUSD); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListActiveProjectMessages(ctx context.Context, owner domain.ProjectOwner, limit int) ([]domain.Message, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	table := "accounts"
	valid := "o.status='active'"
	if owner.IsGuest() {
		table = "guest_sessions"
		valid += " AND o.expires_at>CURRENT_TIMESTAMP(3)"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM (SELECT `+prefixMessageColumns("m")+` FROM `+table+` o JOIN projects p ON p.id=o.active_project_id AND p.`+column+`=o.id AND p.status='active' JOIN conversations c ON c.id=p.active_conversation_id AND c.project_id=p.id AND c.status='active' JOIN messages m ON m.conversation_id=c.id WHERE o.id=? AND `+valid+` ORDER BY m.created_at DESC,m.id DESC LIMIT ?) recent ORDER BY created_at,id`, owner.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}
