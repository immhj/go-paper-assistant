package dao

import (
	"context"
	"errors"

	"paperassistant/internal/domain"
)

var (
	ErrGuestUnavailable        = errors.New("guest session unavailable")
	ErrGuestDeepReadInProgress = errors.New("guest deep read in progress")
	// ErrGuestTaskInProgress remains a source-compatibility alias; public APIs use the new code.
	ErrGuestTaskInProgress    = ErrGuestDeepReadInProgress
	ErrGuestDeepReadExhausted = errors.New("guest deep read exhausted")
	ErrReservationMismatch    = errors.New("guest reservation mismatch")
)

// PlatformStore is the persistence seam for verified accounts and durable guest facts. Methods
// that move guest-owned conversations are transactional; upper layers must not compose those
// operations from table-level calls.
type PlatformStore interface {
	// CreateAccount inserts a verified account and, when guestID is non-nil, atomically transfers
	// that active guest's conversations and revokes the guest credential.
	CreateAccount(ctx context.Context, params domain.CreateAccountParams, guestID *uint64) (domain.Account, domain.GuestImportResult, error)
	GetAccountByEmail(context.Context, string) (domain.Account, error)
	GetAccountByUsername(context.Context, string) (domain.Account, error)
	// ResetPasswordByEmail changes the hash and increments auth_version in one transaction.
	ResetPasswordByEmail(ctx context.Context, email, passwordHash string) (uint64, error)
	UpdateNickname(ctx context.Context, username, nickname string) error
	ListAccounts(context.Context, int) ([]domain.AdminAccountView, error)
	// SetAccountStatus always increments auth_version so every previously issued JWT is revoked.
	SetAccountStatus(context.Context, uint64, string) error

	// CreateGuestSession stores only tokenHash and fixes expiration at creation time + 30 days.
	CreateGuestSession(ctx context.Context, tokenHash []byte) (domain.GuestSession, error)
	GetGuestSessionByTokenHash(ctx context.Context, tokenHash []byte) (domain.GuestSession, error)
	GetResearchSelection(context.Context, domain.ProjectOwner) (domain.ResearchSelection, error)
	HasProjects(context.Context, domain.ProjectOwner) (bool, error)
	// ImportGuestSession performs the narrow completed-import replay check before requiring an
	// active guest. The opaque credential crosses this seam only as a SHA-256 digest.
	ImportGuestSession(ctx context.Context, accountID uint64, tokenHash []byte, idempotencyKey string) (domain.GuestImportResult, error)
	ClearGuestSession(ctx context.Context, guestID uint64) error
	ReserveGuestDeepRead(ctx context.Context, guestID uint64, reservationID string) (domain.GuestSession, error)
	// FinishGuestDeepRead consumes on success and releases only the matching reservation on a
	// confirmed system failure. Callers must not pass success=false for client disconnects.
	FinishGuestDeepRead(ctx context.Context, guestID uint64, reservationID string, success bool) error
	CleanupExpiredGuests(ctx context.Context, limit int) (domain.GuestMetrics, error)
	GuestMetrics(ctx context.Context) (domain.GuestMetrics, error)
}
