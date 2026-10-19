package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

const conversationColumns = `id, user_id, project_id, conversation_no, title, current_context, status, created_at, updated_at`

func (s *Store) CreateConversation(ctx context.Context, userID uint64, title string, current json.RawMessage) (domain.Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "默认会话"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("begin create conversation: %w", err)
	}
	defer tx.Rollback()

	var locked uint64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=? FOR UPDATE`, userID).Scan(&locked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Conversation{}, dao.ErrNotFound
		}
		return domain.Conversation{}, fmt.Errorf("lock user: %w", err)
	}
	var next uint32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(conversation_no),0)+1 FROM conversations WHERE user_id=?`, userID).Scan(&next); err != nil {
		return domain.Conversation{}, fmt.Errorf("next conversation number: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (user_id, conversation_no, title, current_context)
		VALUES (?, ?, ?, ?)`, userID, next, title, nullableJSON(current))
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("insert conversation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("conversation id: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET active_conversation_id=? WHERE id=?`, id, userID); err != nil {
		return domain.Conversation{}, fmt.Errorf("activate conversation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, fmt.Errorf("commit conversation: %w", err)
	}
	return s.GetConversation(ctx, userID, uint64(id))
}

func (s *Store) CreateConversationForMessage(ctx context.Context, userID uint64, title string, current json.RawMessage, qqMessageID, content string) (domain.Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "新会话"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("begin create conversation for message: %w", err)
	}
	defer tx.Rollback()
	var locked uint64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=? FOR UPDATE`, userID).Scan(&locked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Conversation{}, dao.ErrNotFound
		}
		return domain.Conversation{}, fmt.Errorf("lock user: %w", err)
	}
	var next uint32
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(conversation_no),0)+1 FROM conversations WHERE user_id=?`, userID).Scan(&next); err != nil {
		return domain.Conversation{}, fmt.Errorf("next conversation number: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversations (user_id, conversation_no, title, current_context) VALUES (?, ?, ?, ?)`, userID, next, title, nullableJSON(current))
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("insert conversation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("conversation id: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages (conversation_id, role, qq_msg_id, msg_seq, content, delivery_status) VALUES (?, 'user', ?, 0, ?, 'received')`, id, qqMessageID, content); err != nil {
		var mysqlErr *mysqldriver.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return domain.Conversation{}, dao.ErrDuplicate
		}
		return domain.Conversation{}, fmt.Errorf("insert new-conversation message: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET active_conversation_id=? WHERE id=?`, id, userID); err != nil {
		return domain.Conversation{}, fmt.Errorf("activate conversation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, fmt.Errorf("commit conversation: %w", err)
	}
	return s.GetConversation(ctx, userID, uint64(id))
}

func (s *Store) ListConversations(ctx context.Context, userID uint64, limit int) ([]domain.Conversation, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+`
		FROM conversations WHERE user_id=? AND status='active'
		ORDER BY updated_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	return scanConversations(rows)
}

func (s *Store) GetActiveConversation(ctx context.Context, userID uint64) (domain.Conversation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+prefixConversationColumns("c")+`
		FROM users u JOIN conversations c ON c.id=u.active_conversation_id
		WHERE u.id=? AND c.user_id=u.id AND c.status='active'`, userID)
	return scanConversation(row)
}

func (s *Store) GetConversation(ctx context.Context, userID, conversationID uint64) (domain.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+`
		FROM conversations WHERE id=? AND user_id=?`, conversationID, userID))
}

func (s *Store) SwitchConversation(ctx context.Context, userID uint64, number uint32) (domain.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("begin switch conversation: %w", err)
	}
	defer tx.Rollback()
	conversation, err := scanConversation(tx.QueryRowContext(ctx, `SELECT `+conversationColumns+`
		FROM conversations WHERE user_id=? AND conversation_no=? AND status='active' FOR UPDATE`, userID, number))
	if err != nil {
		return domain.Conversation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET active_conversation_id=? WHERE id=?`, conversation.ID, userID); err != nil {
		return domain.Conversation{}, fmt.Errorf("switch conversation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, fmt.Errorf("commit switch: %w", err)
	}
	return conversation, nil
}

func (s *Store) RenameConversation(ctx context.Context, userID, conversationID uint64, title string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET title=? WHERE id=? AND user_id=? AND status='active'`, strings.TrimSpace(title), conversationID, userID)
	if err != nil {
		return fmt.Errorf("rename conversation: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) ArchiveConversation(ctx context.Context, userID uint64, number uint32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin archive conversation: %w", err)
	}
	defer tx.Rollback()
	var id uint64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE user_id=? AND conversation_no=? AND status='active' FOR UPDATE`, userID, number).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return dao.ErrNotFound
		}
		return fmt.Errorf("find conversation: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET status='archived' WHERE id=?`, id); err != nil {
		return fmt.Errorf("archive conversation: %w", err)
	}
	var active sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT active_conversation_id FROM users WHERE id=? FOR UPDATE`, userID).Scan(&active); err != nil {
		return fmt.Errorf("load active conversation: %w", err)
	}
	if active.Valid && uint64(active.Int64) == id {
		var fallback sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE user_id=? AND status='active' ORDER BY updated_at DESC, id DESC LIMIT 1`, userID).Scan(&fallback)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find fallback conversation: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET active_conversation_id=? WHERE id=?`, nullableInt64(fallback), userID); err != nil {
			return fmt.Errorf("set fallback conversation: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) RestoreConversation(ctx context.Context, userID, conversationID uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restore conversation: %w", err)
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM conversations WHERE id=? AND user_id=? FOR UPDATE`, conversationID, userID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return dao.ErrNotFound
		}
		return fmt.Errorf("find archived conversation: %w", err)
	}
	if status == "archived" {
		if _, err = tx.ExecContext(ctx, `UPDATE conversations SET status='active' WHERE id=? AND user_id=?`, conversationID, userID); err != nil {
			return fmt.Errorf("restore conversation: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET active_conversation_id=? WHERE id=? AND active_conversation_id IS NULL`, conversationID, userID); err != nil {
		return fmt.Errorf("activate restored conversation: %w", err)
	}
	return tx.Commit()
}

func (s *Store) UpdateConversationContext(ctx context.Context, userID, conversationID uint64, current json.RawMessage) error {
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET current_context=? WHERE id=? AND user_id=? AND status='active'`, nullableJSON(current), conversationID, userID)
	if err != nil {
		return fmt.Errorf("update conversation context: %w", err)
	}
	return requireAffected(result)
}

func scanConversation(row interface{ Scan(...any) error }) (domain.Conversation, error) {
	var item domain.Conversation
	var current []byte
	var userID, projectID sql.NullString
	if err := row.Scan(&item.ID, &userID, &projectID, &item.ConversationNo,
		&item.Title, &current, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Conversation{}, dao.ErrNotFound
		}
		return domain.Conversation{}, fmt.Errorf("scan conversation: %w", err)
	}
	var err error
	if item.UserID, err = nullableUnsignedPointer(userID); err != nil {
		return domain.Conversation{}, fmt.Errorf("scan conversation user owner: %w", err)
	}
	if item.ProjectID, err = nullableUnsignedPointer(projectID); err != nil {
		return domain.Conversation{}, fmt.Errorf("scan conversation project owner: %w", err)
	}
	if len(current) > 0 {
		item.CurrentContext = append(json.RawMessage(nil), current...)
	}
	if err = item.ValidateOwner(); err != nil {
		return domain.Conversation{}, fmt.Errorf("scan conversation owner invariant: %w", err)
	}
	return item, nil
}

func scanConversations(rows *sql.Rows) ([]domain.Conversation, error) {
	items := make([]domain.Conversation, 0)
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func prefixConversationColumns(prefix string) string {
	parts := strings.Split(conversationColumns, ", ")
	for index := range parts {
		parts[index] = prefix + "." + parts[index]
	}
	return strings.Join(parts, ", ")
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return []byte(value)
}

func nullableUnsignedPointer(value sql.NullString) (*uint64, error) {
	if !value.Valid {
		return nil, nil
	}
	result, err := strconv.ParseUint(value.String, 10, 64)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func uint64Pointer(value sql.NullInt64) *uint64 {
	if !value.Valid {
		return nil
	}
	result := uint64(value.Int64)
	return &result
}

func nullableInt64(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}

func requireAffected(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return dao.ErrNotFound
	}
	return nil
}

func (s *Store) ListAdminConversations(ctx context.Context, userID uint64, limit int) ([]domain.Conversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+`
		FROM conversations WHERE user_id=?
		ORDER BY updated_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list admin conversations: %w", err)
	}
	defer rows.Close()
	return scanConversations(rows)
}
