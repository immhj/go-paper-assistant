package domain

import "time"

// Account is a verified platform login identity. It is distinct from User, which remains the
// anonymous QQ identity used by the paper-assistant channel.
type Account struct {
	ID              uint64     `json:"id"`
	Username        string     `json:"-"`
	Email           string     `json:"email"`
	Nickname        string     `json:"nickname"`
	PasswordHash    string     `json:"-"`
	QQOpenID        *string    `json:"-"`
	Status          string     `json:"status"`
	AuthVersion     uint64     `json:"-"`
	EmailVerifiedAt *time.Time `json:"emailVerifiedAt,omitempty"`
	ActiveProjectID *uint64    `json:"-"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

const (
	AccountStatusActive   = "active"
	AccountStatusDisabled = "disabled"
)

// CreateAccountParams contains only facts that may be persisted after email verification.
// EmailVerifiedAt is a value (rather than a pointer) so callers cannot create a new unverified
// account; Account.EmailVerifiedAt remains nullable for accounts predating verification.
type CreateAccountParams struct {
	Username        string
	Email           string
	Nickname        string
	PasswordHash    string
	EmailVerifiedAt time.Time
}

const (
	GuestLifetime                = 30 * 24 * time.Hour
	GuestReservationCleanupGrace = 24 * time.Hour
)

const (
	GuestStatusActive   = "active"
	GuestStatusUpgraded = "upgraded"
	GuestStatusCleared  = "cleared"
	GuestStatusExpired  = "expired"
)

const (
	GuestDeepReadAvailable = "available"
	GuestDeepReadReserved  = "reserved"
	GuestDeepReadConsumed  = "consumed"
)

// GuestSession is the durable server-side fact behind an opaque guest cookie. TokenHash is
// never serialized or exposed to upper-layer response models.
type GuestSession struct {
	ID                       uint64     `json:"id"`
	TokenHash                []byte     `json:"-"`
	Status                   string     `json:"status"`
	DeepReadState            string     `json:"deepReadState"`
	ReservationID            *string    `json:"-"`
	ReservedAt               *time.Time `json:"reservedAt,omitempty"`
	ExpiresAt                time.Time  `json:"expiresAt"`
	UpgradedToAccountID      *uint64    `json:"-"`
	ActiveProjectID          *uint64    `json:"-"`
	ImportIdempotencyKey     *string    `json:"-"`
	ImportedProjectIDs       []uint64   `json:"-"`
	ImportedCurrentProjectID *uint64    `json:"-"`
	CreatedAt                time.Time  `json:"createdAt"`
	UpdatedAt                time.Time  `json:"updatedAt"`
}

// GuestImportResult exposes stable owner-scoped project numbers, never database IDs.
type GuestImportResult struct {
	Upgraded           bool     `json:"upgraded"`
	AlreadyImported    bool     `json:"alreadyImported,omitempty"`
	ImportedProjectNos []uint32 `json:"importedProjectNos"`
	CurrentProjectNo   *uint32  `json:"currentProjectNo"`
}

// GuestMetrics is both the current-state aggregate and the bounded cleanup result. Cleanup-only
// fields remain zero for a regular metrics query.
type GuestMetrics struct {
	Active          int64 `json:"active"`
	Upgraded        int64 `json:"upgraded"`
	Cleared         int64 `json:"cleared"`
	Expired         int64 `json:"expired"`
	Available       int64 `json:"deepReadAvailable"`
	Reserved        int64 `json:"deepReadReserved"`
	Consumed        int64 `json:"deepReadConsumed"`
	Scanned         int64 `json:"scanned,omitempty"`
	DeletedSessions int64 `json:"deletedSessions,omitempty"`
	DeletedProjects int64 `json:"deletedProjects,omitempty"`
	SkippedReserved int64 `json:"skippedReserved,omitempty"`
	Failures        int64 `json:"failures,omitempty"`
}

// AdminAccountView never carries credential hashes, auth versions, or raw QQ identifiers.
type AdminAccountView struct {
	ID              uint64     `json:"id"`
	Email           string     `json:"email"`
	Nickname        string     `json:"nickname"`
	EmailVerifiedAt *time.Time `json:"emailVerifiedAt,omitempty"`
	QQBound         bool       `json:"qqBound"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
}
