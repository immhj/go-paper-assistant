package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

var _ dao.PlatformStore = (*Store)(nil)

const accountColumns = `id, username, email, nickname, password_hash, qq_openid, status, auth_version, email_verified_at, active_project_id, created_at, updated_at`

const mysqlDuplicateErrno = 1062

// CreateAccount creates a verified account without an invitation gate. If guestID is provided,
// the guest is locked before insertion and all of its conversations are transferred in this
// transaction; no account survives a failed upgrade.
func (s *Store) CreateAccount(ctx context.Context, params domain.CreateAccountParams, guestID *uint64) (domain.Account, domain.GuestImportResult, error) {
	if params.EmailVerifiedAt.IsZero() {
		return domain.Account{}, domain.GuestImportResult{}, errors.New("email verification time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Account{}, domain.GuestImportResult{}, fmt.Errorf("begin create account: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var guest domain.GuestSession
	if guestID != nil {
		guest, err = loadGuestForUpdate(ctx, tx, *guestID)
		if err != nil {
			return domain.Account{}, domain.GuestImportResult{}, err
		}
		if err = validateGuestForTransfer(guest); err != nil {
			return domain.Account{}, domain.GuestImportResult{}, err
		}
	}

	insert, err := tx.ExecContext(ctx, `
		INSERT INTO accounts
			(username, email, nickname, password_hash, status, auth_version, email_verified_at)
		VALUES (?, ?, ?, ?, 'active', 1, ?)`,
		params.Username, params.Email, params.Nickname, params.PasswordHash, params.EmailVerifiedAt)
	if err != nil {
		if isDuplicateOnKey(err, "uk_accounts_email") {
			return domain.Account{}, domain.GuestImportResult{}, dao.ErrDuplicate
		}
		return domain.Account{}, domain.GuestImportResult{}, fmt.Errorf("insert account: %w", err)
	}
	id, err := insert.LastInsertId()
	if err != nil {
		return domain.Account{}, domain.GuestImportResult{}, fmt.Errorf("account id: %w", err)
	}

	importResult := domain.GuestImportResult{ImportedProjectNos: []uint32{}}
	if guestID != nil {
		importResult, err = transferGuestToAccount(ctx, tx, guest, uint64(id), "")
		if err != nil {
			return domain.Account{}, domain.GuestImportResult{}, err
		}
	}
	account, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=?`, id))
	if err != nil {
		return domain.Account{}, domain.GuestImportResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Account{}, domain.GuestImportResult{}, fmt.Errorf("commit create account: %w", err)
	}
	return account, importResult, nil
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (domain.Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE email=?`, email))
}

func (s *Store) GetAccountByUsername(ctx context.Context, username string) (domain.Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE username=?`, username))
}

func (s *Store) ResetPasswordByEmail(ctx context.Context, email, passwordHash string) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin reset password: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id uint64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE email=? FOR UPDATE`, email).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, dao.ErrNotFound
		}
		return 0, fmt.Errorf("lock reset account: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET password_hash=?, auth_version=auth_version+1 WHERE id=?`, passwordHash, id); err != nil {
		return 0, fmt.Errorf("reset password: %w", err)
	}
	var version uint64
	if err = tx.QueryRowContext(ctx, `SELECT auth_version FROM accounts WHERE id=?`, id).Scan(&version); err != nil {
		return 0, fmt.Errorf("read auth version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit reset password: %w", err)
	}
	return version, nil
}

func (s *Store) UpdateNickname(ctx context.Context, username, nickname string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET nickname=? WHERE username=?`, nickname, username)
	if err != nil {
		return fmt.Errorf("update nickname: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) ListAccounts(ctx context.Context, limit int) ([]domain.AdminAccountView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, email, nickname, email_verified_at, qq_openid IS NOT NULL, status, created_at
		  FROM accounts ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	views := make([]domain.AdminAccountView, 0, limit)
	for rows.Next() {
		var view domain.AdminAccountView
		var verified sql.NullTime
		if err = rows.Scan(&view.ID, &view.Email, &view.Nickname, &verified, &view.QQBound, &view.Status, &view.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan account view: %w", err)
		}
		if verified.Valid {
			view.EmailVerifiedAt = &verified.Time
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (s *Store) SetAccountStatus(ctx context.Context, id uint64, status string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET status=?, auth_version=auth_version+1 WHERE id=?`, status, id)
	if err != nil {
		return fmt.Errorf("set account status: %w", err)
	}
	return requireAffected(result)
}

func scanAccount(row interface{ Scan(...any) error }) (domain.Account, error) {
	var account domain.Account
	var openID sql.NullString
	var verified sql.NullTime
	var active sql.NullInt64
	if err := row.Scan(&account.ID, &account.Username, &account.Email, &account.Nickname,
		&account.PasswordHash, &openID, &account.Status, &account.AuthVersion, &verified,
		&active, &account.CreatedAt, &account.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Account{}, dao.ErrNotFound
		}
		return domain.Account{}, fmt.Errorf("scan account: %w", err)
	}
	if openID.Valid {
		account.QQOpenID = &openID.String
	}
	if verified.Valid {
		account.EmailVerifiedAt = &verified.Time
	}
	if active.Valid {
		value := uint64(active.Int64)
		account.ActiveProjectID = &value
	}
	return account, nil
}

func isDuplicateOnKey(err error, key string) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateErrno && strings.Contains(mysqlErr.Message, key)
}
