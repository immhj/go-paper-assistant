package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	mysqldriver "github.com/go-sql-driver/mysql"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

const messageColumns = `id, conversation_id, role, qq_msg_id, qq_outbound_id, msg_seq, content, shown_papers, agent_trace, delivery_status, cost_usd, created_at`

func (s *Store) InsertMessage(ctx context.Context, message domain.Message) (domain.Message, error) {
	seq := uint32(0)
	if message.MessageSeq != nil {
		seq = *message.MessageSeq
	}
	status := message.DeliveryStatus
	if status == "" {
		if message.Role == "user" {
			status = "received"
		} else {
			status = "reserved"
		}
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (conversation_id, role, qq_msg_id, qq_outbound_id, msg_seq, content, shown_papers, agent_trace, delivery_status, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ConversationID, message.Role, nullableString(message.QQMessageID), nullableString(message.QQOutboundID), seq,
		nullableString(message.Content), nullableJSON(message.ShownPapers), nullableJSON(message.AgentTrace), status, message.CostUSD)
	if err != nil {
		var mysqlErr *mysqldriver.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return domain.Message{}, dao.ErrDuplicate
		}
		return domain.Message{}, fmt.Errorf("insert message: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Message{}, fmt.Errorf("message id: %w", err)
	}
	message.ID = uint64(id)
	_, _ = s.db.ExecContext(ctx, `UPDATE conversations SET updated_at=CURRENT_TIMESTAMP(3) WHERE id=?`, message.ConversationID)
	return message, nil
}

// SaveSearchResult confirms a result only after QQ has completed the stream.
// The conversation ordinals and the per-message display record become visible
// atomically, so /read never resolves a candidate that was not delivered.
func (s *Store) SaveSearchResult(ctx context.Context, userID, conversationID uint64, currentContext json.RawMessage, message domain.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search result transaction: %w", err)
	}
	defer tx.Rollback()

	var id uint64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id=? AND user_id=? AND status='active' FOR UPDATE`, conversationID, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return dao.ErrNotFound
		}
		return fmt.Errorf("lock search conversation: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET current_context=?, updated_at=CURRENT_TIMESTAMP(3) WHERE id=?`, currentContext, conversationID); err != nil {
		return fmt.Errorf("update search conversation: %w", err)
	}

	seq := uint32(0)
	if message.MessageSeq != nil {
		seq = *message.MessageSeq
	}
	if message.ID > 0 {
		result, updateErr := tx.ExecContext(ctx, `UPDATE messages SET content=?, shown_papers=?, agent_trace=?, delivery_status='sent', cost_usd=? WHERE id=? AND conversation_id=? AND role='bot'`, nullableString(message.Content), nullableJSON(message.ShownPapers), nullableJSON(message.AgentTrace), message.CostUSD, message.ID, conversationID)
		if updateErr != nil {
			return fmt.Errorf("confirm delivered search message: %w", updateErr)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return dao.ErrNotFound
		}
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO messages (conversation_id, role, qq_msg_id, msg_seq, content, shown_papers, agent_trace, delivery_status, cost_usd)
			VALUES (?, 'bot', ?, ?, ?, ?, ?, 'sent', ?)`,
			conversationID, nullableString(message.QQMessageID), seq, nullableString(message.Content), nullableJSON(message.ShownPapers), nullableJSON(message.AgentTrace), message.CostUSD)
		if err != nil {
			var mysqlErr *mysqldriver.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				return dao.ErrDuplicate
			}
			return fmt.Errorf("insert delivered search message: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit search result: %w", err)
	}
	return nil
}

func (s *Store) HasUserMessage(ctx context.Context, qqMessageID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE qq_msg_id=? AND role='user')`, qqMessageID).Scan(&exists)
	return exists, err
}

func (s *Store) GetUserMessageByQQID(ctx context.Context, qqMessageID string) (domain.Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE qq_msg_id=? AND role='user' LIMIT 1`, qqMessageID))
}

func (s *Store) UpdateMessageContent(ctx context.Context, id uint64, content string, costUSD float64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE messages SET content=?, cost_usd=? WHERE id=? AND role='bot'`, content, costUSD, id)
	if err != nil {
		return fmt.Errorf("update message content: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) UpdateMessageDelivery(ctx context.Context, id uint64, status, outboundID string) error {
	if status != "sent" && status != "failed" && status != "reserved" {
		return errors.New("invalid message delivery status")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE messages SET delivery_status=?, qq_outbound_id=COALESCE(NULLIF(?,''),qq_outbound_id) WHERE id=? AND role='bot'`, status, outboundID, id)
	if err != nil {
		return fmt.Errorf("update message delivery: %w", err)
	}
	return requireAffected(result)
}

// FailReservedMessage is intentionally conditional: a commit whose acknowledgement was lost
// must never be changed from sent back to failed by the recovery path.
func (s *Store) FailReservedMessage(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET delivery_status='failed' WHERE id=? AND role='bot' AND delivery_status='reserved'`, id)
	if err != nil {
		return fmt.Errorf("fail reserved message: %w", err)
	}
	return nil
}

func (s *Store) UpdateMessageContentAndTrace(ctx context.Context, id uint64, content string, costUSD float64, trace json.RawMessage) error {
	result, err := s.db.ExecContext(ctx, `UPDATE messages SET content=?, cost_usd=?, agent_trace=? WHERE id=? AND role='bot'`, content, costUSD, nullableJSON(trace), id)
	if err != nil {
		return fmt.Errorf("update message content and trace: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) UpdateUserMessageAgentTrace(ctx context.Context, qqMessageID string, trace json.RawMessage, costUSD float64) error {
	if len(trace) > 64*1024 || !json.Valid(trace) {
		return errors.New("agent trace must be valid JSON within 64 KiB")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE messages SET agent_trace=?, cost_usd=? WHERE qq_msg_id=? AND role='user'`, nullableJSON(trace), costUSD, qqMessageID)
	if err != nil {
		return fmt.Errorf("update user message agent trace: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) UpdateMessageAgentTraceByID(ctx context.Context, id uint64, trace json.RawMessage, costUSD float64) error {
	if len(trace) > 64*1024 || !json.Valid(trace) {
		return errors.New("agent trace must be valid JSON within 64 KiB")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE messages SET agent_trace=?, cost_usd=? WHERE id=? AND role='user'`, nullableJSON(trace), costUSD, id)
	if err != nil {
		return fmt.Errorf("update message agent trace by id: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) ListRecentUserMessages(ctx context.Context, conversationID uint64, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+`
		FROM messages WHERE conversation_id=? AND role='user'
		ORDER BY created_at DESC, id DESC LIMIT ?`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent messages: %w", err)
	}
	defer rows.Close()
	items, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	return items, nil
}

func (s *Store) CountBotReplies(ctx context.Context, qqMessageID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE qq_msg_id=? AND role='bot'`, qqMessageID).Scan(&count)
	return count, err
}

func (s *Store) FindLatestShownMessage(ctx context.Context, conversationID uint64) (domain.Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+`
		FROM messages WHERE conversation_id=? AND role='bot' AND shown_papers IS NOT NULL AND delivery_status='sent'
		ORDER BY created_at DESC LIMIT 1`, conversationID))
}

func (s *Store) FindShownMessageByQQID(ctx context.Context, conversationID uint64, qqMessageID string) (domain.Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+`
		FROM messages WHERE conversation_id=? AND role='bot' AND qq_outbound_id=? AND shown_papers IS NOT NULL AND delivery_status='sent'
		ORDER BY created_at DESC LIMIT 1`, conversationID, qqMessageID))
}

func (s *Store) FindRecoverableBotReply(ctx context.Context, qqMessageID string) (domain.Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+`
		FROM messages WHERE qq_msg_id=? AND role='bot' AND delivery_status IN ('reserved','failed')
		ORDER BY id DESC LIMIT 1`, qqMessageID))
}

func (s *Store) ListConversationMessages(ctx context.Context, userID, conversationID uint64, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixMessageColumns("m")+`
		FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.user_id=? AND c.id=? ORDER BY m.created_at DESC, m.id DESC LIMIT ?`, userID, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversation messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) ListWebConversationMessages(ctx context.Context, conversationID uint64, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+`
		FROM (
			SELECT `+messageColumns+` FROM messages
			WHERE conversation_id=?
			ORDER BY created_at DESC, id DESC LIMIT ?
		) recent
		ORDER BY created_at, id`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Web conversation messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessage(row interface{ Scan(...any) error }) (domain.Message, error) {
	var item domain.Message
	var qqID, outboundID, content sql.NullString
	var seq sql.NullInt64
	var shown, agentTrace []byte
	if err := row.Scan(&item.ID, &item.ConversationID, &item.Role, &qqID, &outboundID, &seq, &content, &shown, &agentTrace, &item.DeliveryStatus, &item.CostUSD, &item.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Message{}, dao.ErrNotFound
		}
		return domain.Message{}, fmt.Errorf("scan message: %w", err)
	}
	if qqID.Valid {
		item.QQMessageID = &qqID.String
	}
	if outboundID.Valid {
		item.QQOutboundID = &outboundID.String
	}
	if content.Valid {
		item.Content = &content.String
	}
	if seq.Valid && seq.Int64 > 0 {
		value := uint32(seq.Int64)
		item.MessageSeq = &value
	}
	if len(shown) > 0 {
		item.ShownPapers = append(json.RawMessage(nil), shown...)
	}
	if len(agentTrace) > 0 {
		item.AgentTrace = append(json.RawMessage(nil), agentTrace...)
	}
	return item, nil
}

func scanMessages(rows *sql.Rows) ([]domain.Message, error) {
	items := make([]domain.Message, 0)
	for rows.Next() {
		item, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func prefixMessageColumns(prefix string) string {
	return prefix + `.id, ` + prefix + `.conversation_id, ` + prefix + `.role, ` + prefix + `.qq_msg_id, ` + prefix + `.qq_outbound_id, ` +
		prefix + `.msg_seq, ` + prefix + `.content, ` + prefix + `.shown_papers, ` + prefix + `.agent_trace, ` + prefix + `.delivery_status, ` + prefix + `.cost_usd, ` + prefix + `.created_at`
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
