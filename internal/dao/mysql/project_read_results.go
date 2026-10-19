package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

// CommitProjectReadResult atomically turns one reserved Web bot message into a durable
// project read. Fetching, extraction and model generation must finish before this method starts.
func (s *Store) CommitProjectReadResult(ctx context.Context, commit domain.ProjectReadCommit) (domain.ProjectReadResult, error) {
	if err := commit.ProjectOwner.Validate(); err != nil {
		return domain.ProjectReadResult{}, err
	}
	if commit.ProjectNo == 0 || commit.ConversationNo == 0 || commit.PaperID == 0 || commit.MessageID == 0 || strings.TrimSpace(commit.Content) == "" || commit.CostUSD < 0 {
		return domain.ProjectReadResult{}, errors.New("invalid project read commit")
	}
	if commit.ReadType != domain.ProjectReadTypeQuick && commit.ReadType != domain.ProjectReadTypeDeep {
		return domain.ProjectReadResult{}, errors.New("invalid project read type")
	}
	if commit.ProjectOwner.IsGuest() && (commit.GuestReservationID == nil || strings.TrimSpace(*commit.GuestReservationID) == "") {
		return domain.ProjectReadResult{}, errors.New("guest reservation id is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ProjectReadResult{}, fmt.Errorf("begin project read commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	activeProjectID, err := lockProjectOwner(ctx, tx, commit.ProjectOwner)
	if err != nil {
		return domain.ProjectReadResult{}, err
	}

	// Lock the source message before checking current UI pointers. A completed message remains
	// idempotently recoverable even if the user switched projects or conversations after commit.
	var messageConversationID uint64
	var role, deliveryStatus string
	err = tx.QueryRowContext(ctx, `SELECT conversation_id,role,delivery_status FROM messages WHERE id=? FOR UPDATE`, commit.MessageID).Scan(&messageConversationID, &role, &deliveryStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectReadResult{}, dao.ErrNotFound
	}
	if err != nil {
		return domain.ProjectReadResult{}, fmt.Errorf("lock project read message: %w", err)
	}
	if role != "bot" {
		return domain.ProjectReadResult{}, dao.ErrResourceStateConflict
	}

	// The message row lock serializes retries. A completed relation is returned unchanged and
	// neither creates another history row nor consumes the guest reservation a second time.
	existing, existingProjectID, existingPaperID, existingStatus, existingErr := loadCommittedProjectRead(ctx, tx, commit.MessageID)
	if existingErr == nil {
		project, projectErr := loadProjectTx(ctx, tx, commit.ProjectOwner, commit.ProjectNo)
		if projectErr != nil {
			return domain.ProjectReadResult{}, projectErr
		}
		if existingProjectID != project.ID || existingPaperID != commit.PaperID || existing.SourceConversationID != messageConversationID || existing.SourceConversationNo != commit.ConversationNo || existing.ReadType != commit.ReadType || existingStatus != "sent" || deliveryStatus != "sent" {
			return domain.ProjectReadResult{}, dao.ErrResourceStateConflict
		}
		if err = tx.Commit(); err != nil {
			return domain.ProjectReadResult{}, fmt.Errorf("commit project read retry: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return domain.ProjectReadResult{}, existingErr
	}
	if deliveryStatus != "reserved" {
		return domain.ProjectReadResult{}, dao.ErrResourceStateConflict
	}

	project, err := loadProjectTx(ctx, tx, commit.ProjectOwner, commit.ProjectNo)
	if err != nil {
		return domain.ProjectReadResult{}, err
	}
	if project.Status != domain.ProjectStatusActive {
		return domain.ProjectReadResult{}, dao.ErrProjectArchived
	}
	if activeProjectID == nil || *activeProjectID != project.ID {
		return domain.ProjectReadResult{}, dao.ErrResourceStateConflict
	}

	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE project_id=? AND conversation_no=? FOR UPDATE`, project.ID, commit.ConversationNo))
	if errors.Is(err, dao.ErrNotFound) {
		return domain.ProjectReadResult{}, dao.ErrConversationNotFound
	}
	if err != nil {
		return domain.ProjectReadResult{}, err
	}
	if conversation.Status != domain.ConversationStatusActive {
		return domain.ProjectReadResult{}, dao.ErrConversationArchived
	}
	if project.ActiveConversationID == nil || *project.ActiveConversationID != conversation.ID || messageConversationID != conversation.ID {
		return domain.ProjectReadResult{}, dao.ErrResourceStateConflict
	}

	var paperExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM papers WHERE id=?)`, commit.PaperID).Scan(&paperExists); err != nil {
		return domain.ProjectReadResult{}, err
	}
	if !paperExists {
		return domain.ProjectReadResult{}, dao.ErrPaperNotFound
	}

	result, err := tx.ExecContext(ctx, `UPDATE messages SET content=?,cost_usd=?,delivery_status='sent' WHERE id=? AND conversation_id=? AND role='bot' AND delivery_status='reserved'`, commit.Content, commit.CostUSD, commit.MessageID, conversation.ID)
	if err != nil {
		return domain.ProjectReadResult{}, fmt.Errorf("complete project read message: %w", err)
	}
	if err = requireAffected(result); err != nil {
		return domain.ProjectReadResult{}, dao.ErrResourceStateConflict
	}

	var projectPaperID uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM project_papers WHERE project_id=? AND paper_id=? FOR UPDATE`, project.ID, commit.PaperID).Scan(&projectPaperID)
	if errors.Is(err, sql.ErrNoRows) {
		inserted, insertErr := tx.ExecContext(ctx, `INSERT INTO project_papers(project_id,paper_id,status) VALUES(?,?,'active')`, project.ID, commit.PaperID)
		if insertErr != nil {
			return domain.ProjectReadResult{}, fmt.Errorf("insert project paper for read: %w", insertErr)
		}
		id, idErr := inserted.LastInsertId()
		if idErr != nil {
			return domain.ProjectReadResult{}, idErr
		}
		projectPaperID = uint64(id)
	} else if err != nil {
		return domain.ProjectReadResult{}, err
	} else if _, err = tx.ExecContext(ctx, `UPDATE project_papers SET status='active',updated_at=CURRENT_TIMESTAMP(3) WHERE id=?`, projectPaperID); err != nil {
		return domain.ProjectReadResult{}, fmt.Errorf("restore project paper for read: %w", err)
	}

	inserted, err := tx.ExecContext(ctx, `INSERT INTO project_read_results(project_paper_id,message_id,source_conversation_id,read_type) VALUES(?,?,?,?)`, projectPaperID, commit.MessageID, conversation.ID, commit.ReadType)
	if err != nil {
		return domain.ProjectReadResult{}, fmt.Errorf("insert project read result: %w", err)
	}
	readResultID, err := inserted.LastInsertId()
	if err != nil {
		return domain.ProjectReadResult{}, err
	}

	if commit.ProjectOwner.IsGuest() {
		consumed, consumeErr := tx.ExecContext(ctx, `UPDATE guest_sessions SET deep_read_state='consumed' WHERE id=? AND status='active' AND deep_read_state='reserved' AND reservation_id=?`, commit.ProjectOwner.ID, *commit.GuestReservationID)
		if consumeErr != nil {
			return domain.ProjectReadResult{}, fmt.Errorf("consume guest project read reservation: %w", consumeErr)
		}
		affected, affectedErr := consumed.RowsAffected()
		if affectedErr != nil {
			return domain.ProjectReadResult{}, affectedErr
		}
		if affected != 1 {
			return domain.ProjectReadResult{}, dao.ErrReservationMismatch
		}
	}

	completed, _, _, _, err := loadCommittedProjectReadByID(ctx, tx, uint64(readResultID))
	if err != nil {
		return domain.ProjectReadResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ProjectReadResult{}, fmt.Errorf("commit project read: %w", err)
	}
	return completed, nil
}

func loadCommittedProjectRead(ctx context.Context, tx *sql.Tx, messageID uint64) (domain.ProjectReadResult, uint64, uint64, string, error) {
	return scanCommittedProjectRead(tx.QueryRowContext(ctx, committedProjectReadSQL+` WHERE rr.message_id=? FOR UPDATE`, messageID))
}

func loadCommittedProjectReadByID(ctx context.Context, tx *sql.Tx, id uint64) (domain.ProjectReadResult, uint64, uint64, string, error) {
	return scanCommittedProjectRead(tx.QueryRowContext(ctx, committedProjectReadSQL+` WHERE rr.id=?`, id))
}

const committedProjectReadSQL = `SELECT rr.id,rr.project_paper_id,rr.message_id,rr.source_conversation_id,rr.read_type,c.conversation_no,rr.created_at,COALESCE(m.content,''),pp.project_id,pp.paper_id,m.delivery_status FROM project_read_results rr JOIN project_papers pp ON pp.id=rr.project_paper_id JOIN conversations c ON c.id=rr.source_conversation_id JOIN messages m ON m.id=rr.message_id`

func scanCommittedProjectRead(row interface{ Scan(...any) error }) (domain.ProjectReadResult, uint64, uint64, string, error) {
	var item domain.ProjectReadResult
	var projectID, paperID uint64
	var deliveryStatus string
	err := row.Scan(&item.ID, &item.ProjectPaperID, &item.MessageID, &item.SourceConversationID, &item.ReadType, &item.SourceConversationNo, &item.CreatedAt, &item.Content, &projectID, &paperID, &deliveryStatus)
	return item, projectID, paperID, deliveryStatus, err
}

// ListCurrentProjectPapers returns only active relations from the owner's current active project.
// paperIDs is an optional exact batch filter used by project-scoped Agent tools.
func (s *Store) ListCurrentProjectPapers(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, paperIDs []uint64, limit int) ([]domain.ProjectPaper, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return nil, err
	}
	if projectNo == 0 || len(paperIDs) > 100 {
		return nil, errors.New("invalid current project paper query")
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	ownerTable := "accounts"
	ownerValid := "o.status='active'"
	if owner.IsGuest() {
		ownerTable = "guest_sessions"
		ownerValid += " AND o.expires_at>CURRENT_TIMESTAMP(3)"
	}
	query := `SELECT pp.id,pp.project_id,pp.paper_id,p.title,p.publication_date,p.venue_raw,pp.status,pp.created_at,pp.updated_at,
		rr.read_type,rc.conversation_no,rr.created_at
		FROM ` + ownerTable + ` o JOIN projects pr ON pr.id=o.active_project_id AND pr.` + column + `=o.id AND pr.project_no=? AND pr.status='active'
		JOIN project_papers pp ON pp.project_id=pr.id AND pp.status='active' JOIN papers p ON p.id=pp.paper_id
		LEFT JOIN project_read_results rr ON rr.id=(SELECT r.id FROM project_read_results r WHERE r.project_paper_id=pp.id ORDER BY r.created_at DESC,r.id DESC LIMIT 1)
		LEFT JOIN conversations rc ON rc.id=rr.source_conversation_id AND rc.project_id=pr.id
		WHERE o.id=? AND ` + ownerValid
	args := []any{projectNo, owner.ID}
	if len(paperIDs) > 0 {
		placeholders := make([]string, len(paperIDs))
		for index, paperID := range paperIDs {
			if paperID == 0 {
				return nil, errors.New("invalid paper id")
			}
			placeholders[index] = "?"
			args = append(args, paperID)
		}
		query += ` AND pp.paper_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` ORDER BY pp.updated_at DESC,pp.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list current project papers: %w", err)
	}
	defer rows.Close()
	items := make([]domain.ProjectPaper, 0, limit)
	for rows.Next() {
		item, scanErr := scanProjectPaper(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
