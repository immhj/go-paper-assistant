package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

const userColumns = `id, qq_openid, active_conversation_id, last_active_at, created_at`

func (s *Store) UpsertUser(ctx context.Context, openID string) (domain.User, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (qq_openid, last_active_at)
		VALUES (?, CURRENT_TIMESTAMP(3))
		ON DUPLICATE KEY UPDATE last_active_at=CURRENT_TIMESTAMP(3)`, openID)
	if err != nil {
		return domain.User{}, fmt.Errorf("upsert user: %w", err)
	}
	return s.GetUserByOpenID(ctx, openID)
}

func (s *Store) GetUserByID(ctx context.Context, id uint64) (domain.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id))
}

func (s *Store) GetUserByOpenID(ctx context.Context, openID string) (domain.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE qq_openid=?`, openID))
}

func scanUser(row *sql.Row) (domain.User, error) {
	var user domain.User
	var active sql.NullInt64
	if err := row.Scan(&user.ID, &user.QQOpenID, &active, &user.LastActiveAt, &user.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, dao.ErrNotFound
		}
		return domain.User{}, fmt.Errorf("scan user: %w", err)
	}
	if active.Valid {
		value := uint64(active.Int64)
		user.ActiveConversationID = &value
	}
	return user, nil
}

func maskOpenID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:4] + "…" + value[len(value)-4:]
}
