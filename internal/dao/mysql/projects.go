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

const projectColumns = `id, owner_account_id, owner_guest_id, project_no, title, active_conversation_id, status, created_at, updated_at`

func ownerColumn(owner domain.ProjectOwner) (string, error) {
	if err := owner.Validate(); err != nil {
		return "", err
	}
	if owner.IsAccount() {
		return "owner_account_id", nil
	}
	return "owner_guest_id", nil
}

func (s *Store) GetActiveProjectContext(ctx context.Context, owner domain.ProjectOwner) (domain.Project, *domain.Conversation, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return domain.Project{}, nil, err
	}
	ownerTable, pointer := "accounts", "active_project_id"
	validOwner := "o.status='active'"
	if owner.IsGuest() {
		ownerTable = "guest_sessions"
		validOwner += " AND o.expires_at>CURRENT_TIMESTAMP(3)"
	}
	project, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+prefixProjectColumns("p")+`, ac.conversation_no
		FROM `+ownerTable+` o JOIN projects p ON p.id=o.`+pointer+`
		LEFT JOIN conversations ac ON ac.id=p.active_conversation_id AND ac.project_id=p.id AND ac.status='active'
		WHERE o.id=? AND `+validOwner+` AND p.`+column+`=o.id AND p.status='active'`, owner.ID))
	if err != nil {
		return domain.Project{}, nil, err
	}
	if project.ActiveConversationID == nil || project.ActiveConversationNo == nil {
		return project, nil, nil
	}
	conversation, err := scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+`
		FROM conversations WHERE id=? AND project_id=? AND status='active'`, *project.ActiveConversationID, project.ID))
	if errors.Is(err, dao.ErrNotFound) {
		return project, nil, nil
	}
	if err != nil {
		return domain.Project{}, nil, err
	}
	conversation.Selected = true
	return project, &conversation, nil
}

func (s *Store) EnsureActiveProject(ctx context.Context, owner domain.ProjectOwner, ensureConversation bool, current json.RawMessage) (domain.Project, *domain.Conversation, bool, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return domain.Project{}, nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, nil, false, fmt.Errorf("begin ensure active project: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	activeID, err := lockProjectOwner(ctx, tx, owner)
	if err != nil {
		return domain.Project{}, nil, false, err
	}
	var project domain.Project
	created := false
	if activeID != nil {
		project, err = scanProject(tx.QueryRowContext(ctx, `SELECT `+prefixProjectColumns("p")+`, ac.conversation_no FROM projects p
			LEFT JOIN conversations ac ON ac.id=p.active_conversation_id AND ac.project_id=p.id AND ac.status='active'
			WHERE p.id=? AND p.`+column+`=? AND p.status='active' FOR UPDATE`, *activeID, owner.ID))
	}
	if activeID == nil || errors.Is(err, dao.ErrNotFound) {
		if activeID != nil {
			if err = setOwnerActiveProject(ctx, tx, owner, nil); err != nil {
				return domain.Project{}, nil, false, err
			}
		}
		project, err = createProjectTx(ctx, tx, owner, "默认项目", current)
		if err != nil {
			return domain.Project{}, nil, false, err
		}
		created = true
	} else if err != nil {
		return domain.Project{}, nil, false, err
	}

	conversation, err := activeProjectConversationTx(ctx, tx, project)
	if err != nil {
		return domain.Project{}, nil, false, err
	}
	if conversation == nil && ensureConversation {
		value, createErr := createProjectConversationTx(ctx, tx, project.ID, "默认会话", current)
		if createErr != nil {
			return domain.Project{}, nil, false, createErr
		}
		conversation = &value
		project.ActiveConversationID = &value.ID
		project.ActiveConversationNo = &value.ConversationNo
	}
	project.Selected = true
	if err = tx.Commit(); err != nil {
		return domain.Project{}, nil, false, fmt.Errorf("commit ensure active project: %w", err)
	}
	return project, conversation, created, nil
}

func lockProjectOwner(ctx context.Context, tx *sql.Tx, owner domain.ProjectOwner) (*uint64, error) {
	var active sql.NullInt64
	var err error
	if owner.IsAccount() {
		var status string
		err = tx.QueryRowContext(ctx, `SELECT active_project_id,status FROM accounts WHERE id=? FOR UPDATE`, owner.ID).Scan(&active, &status)
		if err == nil && status != domain.AccountStatusActive {
			return nil, dao.ErrNotFound
		}
	} else {
		var status string
		var expires sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT active_project_id,status,expires_at FROM guest_sessions WHERE id=? FOR UPDATE`, owner.ID).Scan(&active, &status, &expires)
		if err == nil && (status != domain.GuestStatusActive || !expires.Valid || !expires.Time.After(timeNow().UTC())) {
			return nil, dao.ErrGuestUnavailable
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, dao.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock project owner: %w", err)
	}
	if active.Valid {
		column, _ := ownerColumn(owner)
		var valid bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=? AND `+column+`=? AND status='active')`, active.Int64, owner.ID).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			if err = setOwnerActiveProject(ctx, tx, owner, nil); err != nil {
				return nil, err
			}
			active.Valid = false
		}
	}
	return uint64Pointer(active), nil
}

func setOwnerActiveProject(ctx context.Context, tx *sql.Tx, owner domain.ProjectOwner, id *uint64) error {
	table := "accounts"
	if owner.IsGuest() {
		table = "guest_sessions"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET active_project_id=? WHERE id=?`, nullableUint64(id), owner.ID); err != nil {
		return fmt.Errorf("set owner active project: %w", err)
	}
	return nil
}

func createProjectTx(ctx context.Context, tx *sql.Tx, owner domain.ProjectOwner, title string, current json.RawMessage) (domain.Project, error) {
	column, _ := ownerColumn(owner)
	var next uint32
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(project_no),0)+1 FROM projects WHERE `+column+`=?`, owner.ID).Scan(&next); err != nil {
		return domain.Project{}, fmt.Errorf("next project number: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO projects (`+column+`,project_no,title) VALUES (?,?,?)`, owner.ID, next, title)
	if err != nil {
		return domain.Project{}, fmt.Errorf("insert project: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Project{}, fmt.Errorf("project id: %w", err)
	}
	conversation, err := createProjectConversationTx(ctx, tx, uint64(id), "默认会话", current)
	if err != nil {
		return domain.Project{}, err
	}
	if err = setOwnerActiveProject(ctx, tx, owner, uint64Ptr(uint64(id))); err != nil {
		return domain.Project{}, err
	}
	project, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projectColumns+`, ? FROM projects WHERE id=?`, conversation.ConversationNo, id))
	if err == nil {
		project.Selected = true
	}
	return project, err
}

func (s *Store) CreateProject(ctx context.Context, owner domain.ProjectOwner, title string, current json.RawMessage) (domain.Project, domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	project, err := createProjectTx(ctx, tx, owner, title, current)
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE id=?`, *project.ActiveConversationID))
	if err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	conversation.Selected = true
	if err = tx.Commit(); err != nil {
		return domain.Project{}, domain.Conversation{}, err
	}
	return project, conversation, nil
}

func (s *Store) ListProjects(ctx context.Context, owner domain.ProjectOwner, status string, limit int, cursor *domain.PagePosition) ([]domain.Project, *domain.PagePosition, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return nil, nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	query := `SELECT ` + prefixProjectColumns("p") + `, ac.conversation_no FROM projects p
		LEFT JOIN conversations ac ON ac.id=p.active_conversation_id AND ac.project_id=p.id AND ac.status='active'
		WHERE p.` + column + `=? AND p.status=?`
	args := []any{owner.ID, status}
	if cursor != nil {
		query += ` AND (p.updated_at<? OR (p.updated_at=? AND p.id<?))`
		args = append(args, cursor.Time, cursor.Time, cursor.ID)
	}
	query += ` ORDER BY p.updated_at DESC,p.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Project, 0, limit+1)
	for rows.Next() {
		item, scanErr := scanProject(rows)
		if scanErr != nil {
			return nil, nil, scanErr
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	return projectPage(items, limit), pageAfterProjects(items, limit), nil
}

func projectPage(items []domain.Project, limit int) []domain.Project {
	if len(items) > limit {
		return items[:limit]
	}
	return items
}
func pageAfterProjects(items []domain.Project, limit int) *domain.PagePosition {
	if len(items) <= limit {
		return nil
	}
	last := items[limit-1]
	return &domain.PagePosition{Time: last.UpdatedAt, ID: last.ID}
}

func (s *Store) RenameProject(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, title string) (domain.Project, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return domain.Project{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE projects SET title=? WHERE `+column+`=? AND project_no=? AND status='active'`, strings.TrimSpace(title), owner.ID, projectNo)
	if err != nil {
		return domain.Project{}, fmt.Errorf("rename project: %w", err)
	}
	if err = requireAffected(result); err != nil {
		project, lookupErr := s.getProject(ctx, owner, projectNo)
		if lookupErr != nil {
			return domain.Project{}, dao.ErrProjectNotFound
		}
		if project.Status == domain.ProjectStatusArchived {
			return domain.Project{}, dao.ErrProjectArchived
		}
		return domain.Project{}, err
	}
	return s.getProject(ctx, owner, projectNo)
}

func (s *Store) getProject(ctx context.Context, owner domain.ProjectOwner, projectNo uint32) (domain.Project, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return domain.Project{}, err
	}
	project, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+prefixProjectColumns("p")+`, ac.conversation_no FROM projects p
		LEFT JOIN conversations ac ON ac.id=p.active_conversation_id AND ac.project_id=p.id AND ac.status='active'
		WHERE p.`+column+`=? AND p.project_no=?`, owner.ID, projectNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Project{}, dao.ErrProjectNotFound
	}
	return project, err
}

func (s *Store) SelectProject(ctx context.Context, owner domain.ProjectOwner, projectNo uint32) (domain.Project, *domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err = lockProjectOwner(ctx, tx, owner); err != nil {
		return domain.Project{}, nil, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Project{}, nil, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Project{}, nil, dao.ErrProjectArchived
	}
	conversation, err := activeProjectConversationTx(ctx, tx, project)
	if err != nil {
		return domain.Project{}, nil, err
	}
	if err = setOwnerActiveProject(ctx, tx, owner, &project.ID); err != nil {
		return domain.Project{}, nil, err
	}
	project.Selected = true
	if err = tx.Commit(); err != nil {
		return domain.Project{}, nil, err
	}
	return project, conversation, nil
}

func (s *Store) ArchiveProject(ctx context.Context, owner domain.ProjectOwner, projectNo uint32) (domain.Project, *domain.Project, *domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, nil, nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	activeID, err := lockProjectOwner(ctx, tx, owner)
	if err != nil {
		return domain.Project{}, nil, nil, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Project{}, nil, nil, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.Project{}, nil, nil, dao.ErrProjectArchived
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET status='archived' WHERE id=?`, project.ID); err != nil {
		return domain.Project{}, nil, nil, err
	}
	project.Status = domain.ProjectStatusArchived
	project.Selected = false
	var fallback *domain.Project
	var conversation *domain.Conversation
	if activeID != nil && *activeID == project.ID {
		column, _ := ownerColumn(owner)
		p, e := scanProject(tx.QueryRowContext(ctx, `SELECT `+prefixProjectColumns("p")+`,ac.conversation_no FROM projects p LEFT JOIN conversations ac ON ac.id=p.active_conversation_id AND ac.project_id=p.id AND ac.status='active' WHERE p.`+column+`=? AND p.status='active' ORDER BY p.updated_at DESC,p.id DESC LIMIT 1 FOR UPDATE`, owner.ID))
		if e != nil && !errors.Is(e, dao.ErrNotFound) {
			return domain.Project{}, nil, nil, e
		}
		if e == nil {
			p.Selected = true
			fallback = &p
			conversation, err = activeProjectConversationTx(ctx, tx, p)
			if err != nil {
				return domain.Project{}, nil, nil, err
			}
			err = setOwnerActiveProject(ctx, tx, owner, &p.ID)
		} else {
			err = setOwnerActiveProject(ctx, tx, owner, nil)
		}
		if err != nil {
			return domain.Project{}, nil, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.Project{}, nil, nil, err
	}
	return project, fallback, conversation, nil
}

func (s *Store) RestoreProject(ctx context.Context, owner domain.ProjectOwner, projectNo uint32) (domain.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	activeID, err := lockProjectOwner(ctx, tx, owner)
	if err != nil {
		return domain.Project{}, err
	}
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return domain.Project{}, err
	}
	if project.Status == domain.ProjectStatusArchived {
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET status='active' WHERE id=?`, project.ID); err != nil {
			return domain.Project{}, err
		}
		project.Status = domain.ProjectStatusActive
	}
	if activeID == nil {
		if err = setOwnerActiveProject(ctx, tx, owner, &project.ID); err != nil {
			return domain.Project{}, err
		}
		project.Selected = true
	}
	if err = tx.Commit(); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func loadProjectTx(ctx context.Context, tx *sql.Tx, owner domain.ProjectOwner, no uint32) (domain.Project, error) {
	column, _ := ownerColumn(owner)
	project, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+prefixProjectColumns("p")+`,ac.conversation_no FROM projects p LEFT JOIN conversations ac ON ac.id=p.active_conversation_id AND ac.project_id=p.id AND ac.status='active' WHERE p.`+column+`=? AND p.project_no=? FOR UPDATE`, owner.ID, no))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.Project{}, dao.ErrProjectNotFound
	}
	if err == nil && project.ActiveConversationID != nil && project.ActiveConversationNo == nil {
		if _, clearErr := tx.ExecContext(ctx, `UPDATE projects SET active_conversation_id=NULL WHERE id=?`, project.ID); clearErr != nil {
			return domain.Project{}, clearErr
		}
		project.ActiveConversationID = nil
	}
	return project, err
}

func activeProjectConversationTx(ctx context.Context, tx *sql.Tx, project domain.Project) (*domain.Conversation, error) {
	if project.ActiveConversationID == nil {
		return nil, nil
	}
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE id=? AND project_id=? AND status='active' FOR UPDATE`, *project.ActiveConversationID, project.ID))
	if errors.Is(err, dao.ErrNotFound) {
		if _, e := tx.ExecContext(ctx, `UPDATE projects SET active_conversation_id=NULL WHERE id=?`, project.ID); e != nil {
			return nil, e
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	conversation.Selected = true
	return &conversation, nil
}

func scanProject(row interface{ Scan(...any) error }) (domain.Project, error) {
	var item domain.Project
	var account, guest, active sql.NullInt64
	var activeNo sql.NullInt64
	if err := row.Scan(&item.ID, &account, &guest, &item.ProjectNo, &item.Title, &active, &item.Status, &item.CreatedAt, &item.UpdatedAt, &activeNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Project{}, dao.ErrNotFound
		}
		return domain.Project{}, fmt.Errorf("scan project: %w", err)
	}
	item.OwnerAccountID = uint64Pointer(account)
	item.OwnerGuestID = uint64Pointer(guest)
	item.ActiveConversationID = uint64Pointer(active)
	if activeNo.Valid {
		v := uint32(activeNo.Int64)
		item.ActiveConversationNo = &v
	}
	if err := item.ValidateOwner(); err != nil {
		return domain.Project{}, fmt.Errorf("scan project owner invariant: %w", err)
	}
	return item, nil
}
func prefixProjectColumns(prefix string) string {
	parts := strings.Split(projectColumns, ", ")
	for i := range parts {
		parts[i] = prefix + "." + parts[i]
	}
	return strings.Join(parts, ", ")
}
func uint64Ptr(v uint64) *uint64 { return &v }

func (s *Store) GetResearchSelection(ctx context.Context, owner domain.ProjectOwner) (domain.ResearchSelection, error) {
	project, conversation, err := s.GetActiveProjectContext(ctx, owner)
	if errors.Is(err, dao.ErrNotFound) {
		return domain.ResearchSelection{}, nil
	}
	if err != nil {
		return domain.ResearchSelection{}, err
	}
	result := domain.ResearchSelection{ActiveProjectNo: &project.ProjectNo}
	if conversation != nil {
		result.ActiveConversationNo = &conversation.ConversationNo
	}
	return result, nil
}
func (s *Store) HasProjects(ctx context.Context, owner domain.ProjectOwner) (bool, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return false, err
	}
	var exists bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE `+column+`=?)`, owner.ID).Scan(&exists)
	return exists, err
}
