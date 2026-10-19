package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

func (s *Store) ListProjectPapers(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, status string, limit int, cursor *domain.PagePosition) ([]domain.ProjectPaper, *domain.PagePosition, error) {
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
	query := `SELECT pp.id,pp.project_id,pp.paper_id,p.title,p.publication_date,p.venue_raw,pp.status,pp.created_at,pp.updated_at,
		rr.read_type,rc.conversation_no,rr.created_at
		FROM projects pr JOIN project_papers pp ON pp.project_id=pr.id JOIN papers p ON p.id=pp.paper_id
		LEFT JOIN project_read_results rr ON rr.id=(SELECT r.id FROM project_read_results r WHERE r.project_paper_id=pp.id ORDER BY r.created_at DESC,r.id DESC LIMIT 1)
		LEFT JOIN conversations rc ON rc.id=rr.source_conversation_id AND rc.project_id=pr.id
		WHERE pr.` + column + `=? AND pr.project_no=? AND pp.status=?`
	args := []any{owner.ID, projectNo, status}
	if cursor != nil {
		query += ` AND (pp.updated_at<? OR (pp.updated_at=? AND pp.id<?))`
		args = append(args, cursor.Time, cursor.Time, cursor.ID)
	}
	query += ` ORDER BY pp.updated_at DESC,pp.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list project papers: %w", err)
	}
	defer rows.Close()
	items := make([]domain.ProjectPaper, 0, limit+1)
	for rows.Next() {
		item, e := scanProjectPaper(rows)
		if e != nil {
			return nil, nil, e
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
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

func (s *Store) AddProjectPaper(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, paperID uint64) (domain.ProjectPaper, bool, error) {
	column, err := ownerColumn(owner)
	if err != nil {
		return domain.ProjectPaper{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ProjectPaper{}, false, err
	}
	defer tx.Rollback()
	var projectID uint64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT id,status FROM projects WHERE `+column+`=? AND project_no=? FOR UPDATE`, owner.ID, projectNo).Scan(&projectID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectPaper{}, false, dao.ErrProjectNotFound
	}
	if err != nil {
		return domain.ProjectPaper{}, false, err
	}
	if status != domain.ProjectStatusActive {
		return domain.ProjectPaper{}, false, dao.ErrProjectArchived
	}
	var paperExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM papers WHERE id=?)`, paperID).Scan(&paperExists); err != nil {
		return domain.ProjectPaper{}, false, err
	}
	if !paperExists {
		return domain.ProjectPaper{}, false, dao.ErrPaperNotFound
	}
	var existingID uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM project_papers WHERE project_id=? AND paper_id=? FOR UPDATE`, projectID, paperID).Scan(&existingID)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return domain.ProjectPaper{}, false, err
	}
	if created {
		result, e := tx.ExecContext(ctx, `INSERT INTO project_papers(project_id,paper_id,status) VALUES(?,?,'active')`, projectID, paperID)
		if e != nil {
			return domain.ProjectPaper{}, false, e
		}
		id, _ := result.LastInsertId()
		existingID = uint64(id)
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE project_papers SET status='active',updated_at=CURRENT_TIMESTAMP(3) WHERE id=?`, existingID); err != nil {
			return domain.ProjectPaper{}, false, err
		}
	}
	item, err := scanProjectPaper(tx.QueryRowContext(ctx, projectPaperByIDSQL, existingID))
	if err != nil {
		return domain.ProjectPaper{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ProjectPaper{}, false, err
	}
	return item, created, nil
}

func (s *Store) RemoveProjectPaper(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, paperID uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	project, err := loadProjectTx(ctx, tx, owner, projectNo)
	if err != nil {
		return err
	}
	if project.Status != domain.ProjectStatusActive {
		return dao.ErrProjectArchived
	}
	var relationID uint64
	err = tx.QueryRowContext(ctx, `SELECT id FROM project_papers WHERE project_id=? AND paper_id=? FOR UPDATE`, project.ID, paperID).Scan(&relationID)
	if errors.Is(err, sql.ErrNoRows) {
		return dao.ErrProjectPaperNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_papers SET status='removed' WHERE id=?`, relationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListProjectReadResults(ctx context.Context, owner domain.ProjectOwner, projectNo uint32, paperID uint64, limit int, cursor *domain.PagePosition) ([]domain.ProjectReadResult, *domain.PagePosition, error) {
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
	query := `SELECT rr.id,rr.project_paper_id,rr.message_id,rr.source_conversation_id,rr.read_type,c.conversation_no,rr.created_at,COALESCE(m.content,'') FROM projects p JOIN project_papers pp ON pp.project_id=p.id JOIN project_read_results rr ON rr.project_paper_id=pp.id JOIN conversations c ON c.id=rr.source_conversation_id AND c.project_id=p.id JOIN messages m ON m.id=rr.message_id AND m.conversation_id=rr.source_conversation_id AND m.role='bot' AND m.delivery_status='sent' WHERE p.` + column + `=? AND p.project_no=? AND pp.paper_id=?`
	args := []any{owner.ID, projectNo, paperID}
	if cursor != nil {
		query += ` AND (rr.created_at<? OR (rr.created_at=? AND rr.id<?))`
		args = append(args, cursor.Time, cursor.Time, cursor.ID)
	}
	query += ` ORDER BY rr.created_at DESC,rr.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]domain.ProjectReadResult, 0, limit+1)
	for rows.Next() {
		var item domain.ProjectReadResult
		if err = rows.Scan(&item.ID, &item.ProjectPaperID, &item.MessageID, &item.SourceConversationID, &item.ReadType, &item.SourceConversationNo, &item.CreatedAt, &item.Content); err != nil {
			return nil, nil, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		var exists bool
		if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects p JOIN project_papers pp ON pp.project_id=p.id WHERE p.`+column+`=? AND p.project_no=? AND pp.paper_id=?)`, owner.ID, projectNo, paperID).Scan(&exists); err != nil {
			return nil, nil, err
		}
		if !exists {
			return nil, nil, dao.ErrProjectPaperNotFound
		}
	}
	var next *domain.PagePosition
	if len(items) > limit {
		last := items[limit-1]
		next = &domain.PagePosition{Time: last.CreatedAt, ID: last.ID}
		items = items[:limit]
	}
	return items, next, nil
}

const projectPaperByIDSQL = `SELECT pp.id,pp.project_id,pp.paper_id,p.title,p.publication_date,p.venue_raw,pp.status,pp.created_at,pp.updated_at,rr.read_type,c.conversation_no,rr.created_at FROM project_papers pp JOIN papers p ON p.id=pp.paper_id LEFT JOIN project_read_results rr ON rr.id=(SELECT r.id FROM project_read_results r WHERE r.project_paper_id=pp.id ORDER BY r.created_at DESC,r.id DESC LIMIT 1) LEFT JOIN conversations c ON c.id=rr.source_conversation_id AND c.project_id=pp.project_id WHERE pp.id=?`

func scanProjectPaper(row interface{ Scan(...any) error }) (domain.ProjectPaper, error) {
	var item domain.ProjectPaper
	var date sql.NullTime
	var venue, readType sql.NullString
	var conversationNo sql.NullInt64
	var readAt sql.NullTime
	if err := row.Scan(&item.ID, &item.ProjectID, &item.PaperID, &item.Title, &date, &venue, &item.Status, &item.CreatedAt, &item.UpdatedAt, &readType, &conversationNo, &readAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ProjectPaper{}, dao.ErrNotFound
		}
		return domain.ProjectPaper{}, err
	}
	if date.Valid {
		item.PublicationDate = &date.Time
	}
	if venue.Valid {
		item.VenueRaw = &venue.String
	}
	if readType.Valid && conversationNo.Valid && readAt.Valid {
		item.LatestReadResult = &domain.ReadResultMeta{ReadType: readType.String, SourceConversationNo: uint32(conversationNo.Int64), CreatedAt: readAt.Time}
	}
	return item, nil
}
