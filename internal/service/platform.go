package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"paperassistant/internal/auth"
	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	mailprovider "paperassistant/internal/provider/mail"
	"paperassistant/internal/provider/ratelimit"
	"paperassistant/internal/provider/verification"
)

type Platform struct {
	store                     dao.PlatformStore
	jwt                       *auth.JWT
	verification              verification.Store
	mailer                    mailprovider.Sender
	now                       func() time.Time
	openRegistrationEnabled   bool
	guestAccessEnabled        bool
	guestDeepReadEnabled      bool
	guestImportEnabled        bool
	emailVerificationRequired bool
	costBreakerEnabled        bool

	loginLimiter    ratelimit.Store
	loginEmailLimit int
	loginIPLimit    int
	loginWindow     time.Duration
}

type PlatformFeatures struct {
	OpenRegistration  bool `json:"openRegistration"`
	GuestAccess       bool `json:"guestAccess"`
	GuestDeepRead     bool `json:"guestDeepRead"`
	GuestImport       bool `json:"guestImport"`
	EmailVerification bool `json:"emailVerification"`
	CostBreaker       bool `json:"costBreaker"`
}

type GuestSessionView struct {
	ExpiresAt     time.Time `json:"expiresAt"`
	DeepReadState string    `json:"deepReadState"`
	HasHistory    bool      `json:"hasHistory"`
}

type SessionView struct {
	ActorType string                   `json:"actorType"`
	Account   *domain.Account          `json:"account,omitempty"`
	Guest     *GuestSessionView        `json:"guest,omitempty"`
	Features  PlatformFeatures         `json:"features"`
	Research  domain.ResearchSelection `json:"research"`
}

type PlatformOption func(*Platform)

func WithOpenRegistration(enabled bool) PlatformOption {
	return func(p *Platform) { p.openRegistrationEnabled = enabled }
}

func WithGuestAccess(enabled bool) PlatformOption {
	return func(p *Platform) { p.guestAccessEnabled = enabled }
}

func WithGuestDeepRead(enabled bool) PlatformOption {
	return func(p *Platform) { p.guestDeepReadEnabled = enabled }
}

func WithGuestImport(enabled bool) PlatformOption {
	return func(p *Platform) { p.guestImportEnabled = enabled }
}

func WithCostBreaker(enabled bool) PlatformOption {
	return func(p *Platform) { p.costBreakerEnabled = enabled }
}

func WithEmailVerificationRequired(required bool) PlatformOption {
	return func(p *Platform) { p.emailVerificationRequired = required }
}

// WithLoginThrottle locks an email after emailLimit failures and a client IP after ipLimit
// failures within window. Without it Login is unthrottled (tests only).
func WithLoginThrottle(store ratelimit.Store, emailLimit, ipLimit int, window time.Duration) PlatformOption {
	return func(p *Platform) {
		p.loginLimiter, p.loginEmailLimit, p.loginIPLimit, p.loginWindow = store, emailLimit, ipLimit, window
	}
}

func WithPlatformClock(now func() time.Time) PlatformOption {
	return func(p *Platform) {
		if now != nil {
			p.now = now
		}
	}
}

func NewPlatform(store dao.PlatformStore, jwt *auth.JWT, verificationStore verification.Store, mailer mailprovider.Sender, options ...PlatformOption) *Platform {
	platform := &Platform{
		store: store, jwt: jwt, verification: verificationStore, mailer: mailer,
		now: time.Now, openRegistrationEnabled: true, guestAccessEnabled: true,
		guestDeepReadEnabled: true, guestImportEnabled: true,
		emailVerificationRequired: true, costBreakerEnabled: true,
	}
	for _, option := range options {
		option(platform)
	}
	return platform
}

var (
	ErrValidation            = errors.New("validation failed")
	ErrEmailTaken            = errors.New("email already registered")
	ErrInvalidCredentials    = errors.New("invalid credentials")
	ErrInvalidToken          = errors.New("invalid token")
	ErrRegistrationClosed    = errors.New("open registration disabled")
	ErrEmailDelivery         = errors.New("email delivery unavailable")
	ErrDependencyUnavailable = errors.New("dependency unavailable")
	ErrGuestAccessDisabled   = errors.New("guest access disabled")
	ErrGuestImportDisabled   = errors.New("guest import disabled")
	ErrGuestUnavailable      = errors.New("guest session unavailable")

	ErrThrottled                     = errors.New("too many attempts")
	ErrVerificationCodeInvalid       = verification.ErrInvalidCode
	ErrVerificationExpired           = verification.ErrExpired
	ErrVerificationAttemptsExhausted = verification.ErrAttemptsExhausted
	ErrVerificationRateLimited       = verification.ErrResendTooSoon
)

// ThrottledError reports a rate-limit rejection together with how long the caller should wait.
// It matches ErrThrottled with errors.Is.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string        { return ErrThrottled.Error() }
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

type AuthResult struct {
	Token                string                   `json:"token"`
	Account              domain.Account           `json:"account"`
	GuestUpgrade         domain.GuestImportResult `json:"guestUpgrade,omitempty"`
	GuestImportAvailable bool                     `json:"guestImportAvailable,omitempty"`
}

type RegistrationCodeResult struct {
	RegistrationToken string    `json:"registrationToken"`
	ExpiresAt         time.Time `json:"expiresAt"`
	ResendAfter       time.Time `json:"resendAfter"`
}

type PasswordResetCodeResult struct {
	ResetToken  string    `json:"resetToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	ResendAfter time.Time `json:"resendAfter"`
}

const minPasswordLen = 8

// RequestRegistrationCode validates and hashes the password before any Redis write, then starts a
// pending challenge. Only successful delivery activates the challenge; failed delivery is
// CAS-cancelled together with its cooldown and rate reservations.
func (p *Platform) RequestRegistrationCode(ctx context.Context, email, nickname, password, clientIP string) (RegistrationCodeResult, error) {
	if !p.openRegistrationEnabled {
		return RegistrationCodeResult{}, ErrRegistrationClosed
	}
	email = normalizeEmail(email)
	nickname = strings.TrimSpace(nickname)
	if !looksLikeEmail(email) || nickname == "" || len([]rune(nickname)) > 64 || len(password) < minPasswordLen {
		return RegistrationCodeResult{}, ErrValidation
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return RegistrationCodeResult{}, err
	}
	challenge, err := p.verification.StartRegistration(ctx, verification.RegistrationData{
		Email: email, Nickname: nickname, PasswordHash: passwordHash,
	}, clientIP)
	if err != nil {
		return RegistrationCodeResult{}, mapVerificationDependency(err)
	}
	if err := p.mailer.SendRegistrationCode(ctx, email, challenge.Code); err != nil {
		_ = p.verification.Cancel(ctx, verification.PurposeRegistration, challenge.Token)
		return RegistrationCodeResult{}, ErrEmailDelivery
	}
	if err := p.verification.Activate(ctx, verification.PurposeRegistration, challenge.Token); err != nil {
		_ = p.verification.Cancel(ctx, verification.PurposeRegistration, challenge.Token)
		return RegistrationCodeResult{}, mapVerificationDependency(err)
	}
	return RegistrationCodeResult{
		RegistrationToken: challenge.Token, ExpiresAt: challenge.ExpiresAt, ResendAfter: challenge.ResendAfter,
	}, nil
}

// Register claims a delivered registration challenge, creates the verified account (and optional
// guest upgrade) transactionally in MySQL, then consumes the challenge.
func (p *Platform) Register(ctx context.Context, registrationToken, code string, guestID *uint64) (AuthResult, error) {
	claim, err := p.verification.VerifyRegistration(ctx, strings.TrimSpace(registrationToken), strings.TrimSpace(code))
	if err != nil {
		return AuthResult{}, mapVerificationDependency(err)
	}
	username, err := generateUsername()
	if err != nil {
		_ = p.verification.Release(ctx, claim)
		return AuthResult{}, err
	}
	account, guestUpgrade, err := p.store.CreateAccount(ctx, domain.CreateAccountParams{
		Username: username, Email: claim.Registration.Email, Nickname: claim.Registration.Nickname,
		PasswordHash: claim.Registration.PasswordHash, EmailVerifiedAt: p.now(),
	}, guestID)
	if err != nil {
		if errors.Is(err, dao.ErrDuplicate) {
			_ = p.verification.Consume(ctx, claim)
			return AuthResult{}, ErrEmailTaken
		}
		_ = p.verification.Release(ctx, claim)
		return AuthResult{}, err
	}
	_ = p.verification.Consume(ctx, claim)
	result, err := p.issue(account)
	if err != nil {
		return AuthResult{}, err
	}
	result.GuestUpgrade = guestUpgrade
	return result, nil
}

// Login throttles failed attempts per email and per client IP. The throttle fails open: if Redis
// is unavailable existing accounts can still authenticate (the argon2 concurrency cap in auth
// still bounds the cost of a flood), and the outage is logged.
func (p *Platform) Login(ctx context.Context, email, password, clientIP string) (AuthResult, error) {
	email = normalizeEmail(email)
	emailKey, ipKey := ratelimit.Key("login:email", email), ratelimit.Key("login:ip", clientIP)
	if err := p.checkLoginThrottle(ctx, emailKey, ipKey); err != nil {
		return AuthResult{}, err
	}

	account, err := p.store.GetAccountByEmail(ctx, email)
	if err != nil && !errors.Is(err, dao.ErrNotFound) {
		return AuthResult{}, err
	}
	// Always spend one argon2id verification, even for unknown or disabled accounts, so
	// response time does not reveal which emails are registered.
	var passwordOK bool
	if err == nil {
		passwordOK = auth.VerifyPassword(account.PasswordHash, password)
	} else {
		passwordOK = auth.VerifyDummyPassword(password)
	}
	// Grandfathered migration compatibility: accounts created before email verification was
	// introduced retain access when email_verified_at is NULL. Every account created by the new
	// registration path is still necessarily verified by CreateAccountParams.
	if err != nil || !passwordOK || account.Status != domain.AccountStatusActive {
		p.recordLoginFailure(ctx, emailKey, ipKey)
		return AuthResult{}, ErrInvalidCredentials
	}
	if p.loginLimiter != nil {
		if resetErr := p.loginLimiter.Reset(ctx, emailKey); resetErr != nil {
			slog.Warn("login throttle reset failed", "error", resetErr)
		}
	}
	return p.issue(account)
}

func (p *Platform) checkLoginThrottle(ctx context.Context, emailKey, ipKey string) error {
	if p.loginLimiter == nil {
		return nil
	}
	for _, check := range []struct {
		key   string
		limit int
	}{{emailKey, p.loginEmailLimit}, {ipKey, p.loginIPLimit}} {
		count, ttl, err := p.loginLimiter.Count(ctx, check.key)
		if err != nil {
			slog.Warn("login throttle unavailable; allowing attempt", "error", err)
			return nil
		}
		if count >= int64(check.limit) {
			return &ThrottledError{RetryAfter: ttl}
		}
	}
	return nil
}

func (p *Platform) recordLoginFailure(ctx context.Context, emailKey, ipKey string) {
	if p.loginLimiter == nil {
		return
	}
	for _, key := range []string{emailKey, ipKey} {
		if _, _, err := p.loginLimiter.Hit(ctx, key, p.loginWindow); err != nil {
			slog.Warn("login throttle record failed", "error", err)
			return
		}
	}
}

// RequestPasswordReset applies the same Redis cooldown and mail budgets to real and decoy
// requests. Only an active verified account receives mail; failed delivery is replaced with an
// indistinguishable, non-verifiable decoy challenge.
func (p *Platform) RequestPasswordReset(ctx context.Context, email, clientIP string) (PasswordResetCodeResult, error) {
	email = normalizeEmail(email)
	account, err := p.store.GetAccountByEmail(ctx, email)
	if err != nil && !errors.Is(err, dao.ErrNotFound) {
		return PasswordResetCodeResult{}, err
	}
	real := err == nil && account.Status == domain.AccountStatusActive && account.EmailVerifiedAt != nil
	resetEmail := email
	if real {
		resetEmail = account.Email
	}
	challenge, err := p.verification.StartReset(ctx, verification.ResetData{Email: resetEmail, Valid: real}, clientIP)
	if err != nil {
		return PasswordResetCodeResult{}, mapVerificationDependency(err)
	}
	if real {
		if err := p.mailer.SendPasswordResetCode(ctx, account.Email, challenge.Code); err != nil {
			_ = p.verification.Cancel(ctx, verification.PurposeReset, challenge.Token)
			challenge, err = p.verification.StartReset(ctx, verification.ResetData{Email: resetEmail, Valid: false}, clientIP)
			if err != nil {
				return PasswordResetCodeResult{}, mapVerificationDependency(err)
			}
		}
	}
	if err := p.verification.Activate(ctx, verification.PurposeReset, challenge.Token); err != nil {
		_ = p.verification.Cancel(ctx, verification.PurposeReset, challenge.Token)
		return PasswordResetCodeResult{}, mapVerificationDependency(err)
	}
	return PasswordResetCodeResult{
		ResetToken: challenge.Token, ExpiresAt: challenge.ExpiresAt, ResendAfter: challenge.ResendAfter,
	}, nil
}

func (p *Platform) ResetPassword(ctx context.Context, resetToken, code, newPassword string) error {
	if len(newPassword) < minPasswordLen {
		return ErrValidation
	}
	claim, err := p.verification.VerifyReset(ctx, strings.TrimSpace(resetToken), strings.TrimSpace(code))
	if err != nil {
		return mapVerificationDependency(err)
	}
	if !claim.Reset.Valid {
		_ = p.verification.Consume(ctx, claim)
		return ErrVerificationExpired
	}
	passwordHash, err := auth.HashPassword(newPassword)
	if err != nil {
		_ = p.verification.Release(ctx, claim)
		return err
	}
	if _, err := p.store.ResetPasswordByEmail(ctx, claim.Reset.Email, passwordHash); err != nil {
		if errors.Is(err, dao.ErrNotFound) {
			_ = p.verification.Consume(ctx, claim)
			return ErrVerificationExpired
		}
		_ = p.verification.Release(ctx, claim)
		return err
	}
	_ = p.verification.Consume(ctx, claim)
	return nil
}

// Authenticate cryptographically verifies the token, then checks the durable account status and
// auth_version. A present but stale/disabled token is never downgraded to a guest identity.
func (p *Platform) Authenticate(ctx context.Context, token string) (domain.Account, error) {
	claims, err := p.jwt.Verify(strings.TrimSpace(token))
	if err != nil {
		return domain.Account{}, ErrInvalidToken
	}
	account, err := p.store.GetAccountByUsername(ctx, claims.Username)
	if err != nil {
		if errors.Is(err, dao.ErrNotFound) {
			return domain.Account{}, ErrInvalidToken
		}
		return domain.Account{}, err
	}
	// Grandfathered migration compatibility: a NULL verification timestamp identifies a
	// pre-migration account, not a newly-created unverified account (the new write path forbids it).
	if account.Status != domain.AccountStatusActive || account.AuthVersion != claims.AuthVersion {
		return domain.Account{}, ErrInvalidToken
	}
	return account, nil
}

func (p *Platform) issue(account domain.Account) (AuthResult, error) {
	token, err := p.jwt.Issue(account.Username, account.AuthVersion)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Token: token, Account: account}, nil
}

func mapVerificationDependency(err error) error {
	if errors.Is(err, verification.ErrUnavailable) {
		return fmt.Errorf("%w: %v", ErrDependencyUnavailable, err)
	}
	return err
}

func (p *Platform) Features() PlatformFeatures {
	return PlatformFeatures{
		OpenRegistration: p.openRegistrationEnabled, GuestAccess: p.guestAccessEnabled,
		GuestDeepRead: p.guestDeepReadEnabled, GuestImport: p.guestImportEnabled,
		EmailVerification: p.emailVerificationRequired, CostBreaker: p.costBreakerEnabled,
	}
}

func (p *Platform) AccountSession(ctx context.Context, account domain.Account) SessionView {
	selection, _ := p.store.GetResearchSelection(ctx, domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: account.ID})
	return SessionView{ActorType: WebActorAccount, Account: &account, Features: p.Features(), Research: selection}
}

// CreateGuest creates an opaque 32-byte URL-safe credential. Only its SHA-256 digest crosses
// the DAO boundary; callers must place the returned plaintext token exclusively in HttpOnly cookie state.
func (p *Platform) CreateGuest(ctx context.Context) (string, domain.GuestSession, error) {
	if !p.guestAccessEnabled {
		return "", domain.GuestSession{}, ErrGuestAccessDisabled
	}
	for attempt := 0; attempt < 3; attempt++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", domain.GuestSession{}, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		digest := sha256.Sum256([]byte(token))
		guest, err := p.store.CreateGuestSession(ctx, digest[:])
		if errors.Is(err, dao.ErrDuplicate) {
			continue
		}
		return token, guest, err
	}
	return "", domain.GuestSession{}, errors.New("could not allocate guest credential")
}

// ResolveGuest hashes an opaque credential before lookup and accepts only active, unexpired facts.
func (p *Platform) ResolveGuest(ctx context.Context, token string) (domain.GuestSession, error) {
	digest, err := guestTokenDigest(token)
	if err != nil {
		return domain.GuestSession{}, ErrGuestUnavailable
	}
	guest, err := p.store.GetGuestSessionByTokenHash(ctx, digest)
	if err != nil {
		if errors.Is(err, dao.ErrNotFound) {
			return domain.GuestSession{}, ErrGuestUnavailable
		}
		return domain.GuestSession{}, err
	}
	if guest.Status != domain.GuestStatusActive || !guest.ExpiresAt.After(p.now()) {
		return domain.GuestSession{}, ErrGuestUnavailable
	}
	return guest, nil
}

func (p *Platform) GuestSession(ctx context.Context, guest domain.GuestSession) SessionView {
	owner := domain.ProjectOwner{Type: domain.ProjectOwnerGuest, ID: guest.ID}
	selection, _ := p.store.GetResearchSelection(ctx, owner)
	hasProjects, _ := p.store.HasProjects(ctx, owner)
	return SessionView{
		ActorType: WebActorGuest,
		Guest:     &GuestSessionView{ExpiresAt: guest.ExpiresAt, DeepReadState: guest.DeepReadState, HasHistory: hasProjects},
		Features:  p.Features(), Research: selection,
	}
}

func (p *Platform) ClearGuest(ctx context.Context, token string) error {
	guest, err := p.ResolveGuest(ctx, token)
	if err != nil {
		return err
	}
	return p.store.ClearGuestSession(ctx, guest.ID)
}

func (p *Platform) ImportGuest(ctx context.Context, accountID uint64, token, idempotencyKey string) (domain.GuestImportResult, error) {
	if !p.guestImportEnabled {
		return domain.GuestImportResult{}, ErrGuestImportDisabled
	}
	digest, err := guestTokenDigest(token)
	if err != nil {
		return domain.GuestImportResult{}, ErrGuestUnavailable
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return domain.GuestImportResult{}, ErrValidation
	}
	return p.store.ImportGuestSession(ctx, accountID, digest, strings.TrimSpace(idempotencyKey))
}

func (p *Platform) GuestImportAvailable(ctx context.Context, token string) bool {
	guest, err := p.ResolveGuest(ctx, token)
	if err != nil || !p.guestImportEnabled {
		return false
	}
	hasProjects, _ := p.store.HasProjects(ctx, domain.ProjectOwner{Type: domain.ProjectOwnerGuest, ID: guest.ID})
	return hasProjects
}

func (p *Platform) GuestMetrics(ctx context.Context) (domain.GuestMetrics, error) {
	return p.store.GuestMetrics(ctx)
}

func (p *Platform) CleanupExpiredGuests(ctx context.Context, limit int) (domain.GuestMetrics, error) {
	return p.store.CleanupExpiredGuests(ctx, limit)
}

func (p *Platform) ListAccounts(ctx context.Context, limit int) ([]domain.AdminAccountView, error) {
	return p.store.ListAccounts(ctx, clampLimit(limit))
}

func (p *Platform) SetAccountStatus(ctx context.Context, id uint64, status string) error {
	if status != domain.AccountStatusActive && status != domain.AccountStatusDisabled {
		return ErrValidation
	}
	return p.store.SetAccountStatus(ctx, id, status)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func looksLikeEmail(email string) bool {
	if email == "" || len(email) > 191 || strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	at := strings.IndexByte(email, '@')
	return at > 0 && at < len(email)-1 && strings.IndexByte(email[at+1:], '@') < 0 && strings.Contains(email[at+1:], ".")
}

func clampLimit(limit int) int {
	if limit < 1 || limit > 500 {
		return 100
	}
	return limit
}

func generateUsername() (string, error) {
	suffix, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return "u_" + suffix, nil
}

// randomHex is shared by platform-owned opaque identifiers in this package.
func randomHex(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}

// Account preserves the existing profile lookup seam for callers that already hold a validated
// username. New authentication middleware should call Authenticate before placing it in context.
func (p *Platform) Account(ctx context.Context, username string) (domain.Account, error) {
	return p.store.GetAccountByUsername(ctx, username)
}

func (p *Platform) UpdateNickname(ctx context.Context, username, nickname string) (domain.Account, error) {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" || len([]rune(nickname)) > 64 {
		return domain.Account{}, ErrValidation
	}
	if err := p.store.UpdateNickname(ctx, username, nickname); err != nil {
		return domain.Account{}, err
	}
	return p.store.GetAccountByUsername(ctx, username)
}

func guestTokenDigest(token string) ([]byte, error) {
	token = strings.TrimSpace(token)
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return nil, ErrGuestUnavailable
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], nil
}
