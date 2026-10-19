package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"paperassistant/internal/auth"
	"paperassistant/internal/dao"
	"paperassistant/internal/dao/daotest"
	"paperassistant/internal/domain"
	mailprovider "paperassistant/internal/provider/mail"
	"paperassistant/internal/provider/verification"
	"paperassistant/internal/service"
)

type platformFixture struct {
	platform     *service.Platform
	store        *daotest.FakePlatformStore
	verification *verification.Fake
	mailer       *mailprovider.FakeSender
	jwt          *auth.JWT
	now          time.Time
}

func newPlatform(t *testing.T) *platformFixture {
	t.Helper()
	fixture := &platformFixture{now: time.Unix(1_700_000_000, 0)}
	fixture.store = daotest.NewFakePlatformStore()
	fixture.store.SetNow(func() time.Time { return fixture.now })
	fixture.verification = verification.NewFake(verification.Config{
		Pepper: []byte("0123456789abcdef0123456789abcdef"),
		Now:    func() time.Time { return fixture.now },
	})
	fixture.mailer = &mailprovider.FakeSender{}
	fixture.jwt = auth.NewJWT("test-secret", time.Hour)
	fixture.platform = service.NewPlatform(
		fixture.store, fixture.jwt, fixture.verification, fixture.mailer,
		service.WithPlatformClock(func() time.Time { return fixture.now }),
	)
	return fixture
}

func (f *platformFixture) requestRegistration(t *testing.T) (service.RegistrationCodeResult, mailprovider.Message) {
	t.Helper()
	result, err := f.platform.RequestRegistrationCode(
		context.Background(), "Alice@Example.com", "Alice", "hunter2hunter", "192.0.2.1",
	)
	if err != nil {
		t.Fatalf("request registration code: %v", err)
	}
	message, ok := f.mailer.Last()
	if !ok {
		t.Fatal("registration email was not sent")
	}
	return result, message
}

func (f *platformFixture) register(t *testing.T) service.AuthResult {
	t.Helper()
	request, message := f.requestRegistration(t)
	result, err := f.platform.Register(context.Background(), request.RegistrationToken, message.Code, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return result
}

func TestRegistrationCreatesAccountOnlyAfterVerification(t *testing.T) {
	fixture := newPlatform(t)
	request, message := fixture.requestRegistration(t)
	if _, err := fixture.store.GetAccountByEmail(context.Background(), "alice@example.com"); !errors.Is(err, dao.ErrNotFound) {
		t.Fatalf("pending registration created account: %v", err)
	}
	if _, err := fixture.platform.Register(context.Background(), request.RegistrationToken, "000000", nil); !errors.Is(err, service.ErrVerificationCodeInvalid) {
		t.Fatalf("wrong code: got %v", err)
	}
	result, err := fixture.platform.Register(context.Background(), request.RegistrationToken, message.Code, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if result.Account.Email != "alice@example.com" || result.Account.EmailVerifiedAt == nil {
		t.Fatalf("account not normalized/verified: %+v", result.Account)
	}
	claims, err := fixture.jwt.Verify(result.Token)
	if err != nil {
		t.Fatalf("verify issued JWT: %v", err)
	}
	if claims.Username != result.Account.Username || claims.AuthVersion != result.Account.AuthVersion {
		t.Fatalf("JWT claims do not match account: %+v / %+v", claims, result.Account)
	}
}

func TestRegistrationSMTPFailureCancelsChallengeAndCooldown(t *testing.T) {
	fixture := newPlatform(t)
	fixture.mailer.Err = errors.New("smtp unavailable")
	if _, err := fixture.platform.RequestRegistrationCode(
		context.Background(), "alice@example.com", "Alice", "hunter2hunter", "192.0.2.1",
	); !errors.Is(err, service.ErrEmailDelivery) {
		t.Fatalf("want email delivery error, got %v", err)
	}
	if _, err := fixture.store.GetAccountByEmail(context.Background(), "alice@example.com"); !errors.Is(err, dao.ErrNotFound) {
		t.Fatalf("SMTP failure created account: %v", err)
	}
	fixture.mailer.Err = nil
	if _, err := fixture.platform.RequestRegistrationCode(
		context.Background(), "alice@example.com", "Alice", "hunter2hunter", "192.0.2.1",
	); err != nil {
		t.Fatalf("failed SMTP attempt left cooldown/challenge: %v", err)
	}
}

func TestRegistrationCanAtomicallyUpgradeGuest(t *testing.T) {
	fixture := newPlatform(t)
	guest, err := fixture.store.CreateGuestSession(context.Background(), make([]byte, 32))
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	conversationID := uint64(44)
	if err := fixture.store.SeedGuestProjects(guest.ID, []uint64{44}, &conversationID); err != nil {
		t.Fatalf("seed guest conversations: %v", err)
	}
	request, message := fixture.requestRegistration(t)
	result, err := fixture.platform.Register(context.Background(), request.RegistrationToken, message.Code, &guest.ID)
	if err != nil {
		t.Fatalf("register with guest upgrade: %v", err)
	}
	if !result.GuestUpgrade.Upgraded || len(result.GuestUpgrade.ImportedProjectNos) != 1 {
		t.Fatalf("guest was not atomically upgraded: %+v", result.GuestUpgrade)
	}
}

func TestLoginDoesNotDependOnVerificationStore(t *testing.T) {
	fixture := newPlatform(t)
	fixture.register(t)
	// The account login path has no verification Store call and remains available after the
	// registration challenge has already been consumed.
	result, err := fixture.platform.Login(context.Background(), "ALICE@example.com", "hunter2hunter")
	if err != nil || result.Token == "" {
		t.Fatalf("valid login failed: result=%+v err=%v", result, err)
	}
	for _, attempt := range []struct{ email, password string }{
		{"alice@example.com", "wrongwrong"}, {"missing@example.com", "hunter2hunter"},
	} {
		if _, err := fixture.platform.Login(context.Background(), attempt.email, attempt.password); !errors.Is(err, service.ErrInvalidCredentials) {
			t.Fatalf("credentials %+v: want generic error, got %v", attempt, err)
		}
	}
}

func TestResetPasswordRevokesOldJWTAndNewLoginWorks(t *testing.T) {
	fixture := newPlatform(t)
	registered := fixture.register(t)
	oldToken := registered.Token

	fixture.now = fixture.now.Add(time.Minute)
	request, err := fixture.platform.RequestPasswordReset(context.Background(), "alice@example.com", "192.0.2.1")
	if err != nil {
		t.Fatalf("request reset: %v", err)
	}
	message, ok := fixture.mailer.Last()
	if !ok || message.Purpose != "password_reset" {
		t.Fatalf("reset email missing: %+v", message)
	}
	if err := fixture.platform.ResetPassword(context.Background(), request.ResetToken, message.Code, "newpassword123"); err != nil {
		t.Fatalf("reset password: %v", err)
	}
	if _, err := fixture.platform.Authenticate(context.Background(), oldToken); !errors.Is(err, service.ErrInvalidToken) {
		t.Fatalf("old JWT survived password reset: %v", err)
	}
	if _, err := fixture.platform.Login(context.Background(), "alice@example.com", "hunter2hunter"); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	newLogin, err := fixture.platform.Login(context.Background(), "alice@example.com", "newpassword123")
	if err != nil {
		t.Fatalf("new password login: %v", err)
	}
	if _, err := fixture.platform.Authenticate(context.Background(), newLogin.Token); err != nil {
		t.Fatalf("new JWT authentication: %v", err)
	}
}

func TestAuthenticateChecksCurrentStatusAndAuthVersion(t *testing.T) {
	fixture := newPlatform(t)
	registered := fixture.register(t)
	if _, err := fixture.platform.Authenticate(context.Background(), registered.Token); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	if err := fixture.platform.SetAccountStatus(context.Background(), registered.Account.ID, domain.AccountStatusDisabled); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	if _, err := fixture.platform.Authenticate(context.Background(), registered.Token); !errors.Is(err, service.ErrInvalidToken) {
		t.Fatalf("disabled/version-bumped account token accepted: %v", err)
	}
}

func TestUnknownResetEmailGetsSameShapeWithoutDelivery(t *testing.T) {
	fixture := newPlatform(t)
	result, err := fixture.platform.RequestPasswordReset(context.Background(), "missing@example.com", "192.0.2.1")
	if err != nil {
		t.Fatalf("unknown reset request: %v", err)
	}
	if len(result.ResetToken) != 43 || result.ExpiresAt.IsZero() || result.ResendAfter.IsZero() {
		t.Fatalf("pseudo reset shape differs: %+v", result)
	}
	if _, ok := fixture.mailer.Last(); ok {
		t.Fatal("unknown email received a message")
	}
	if err := fixture.platform.ResetPassword(context.Background(), result.ResetToken, "123456", "newpassword123"); !errors.Is(err, service.ErrVerificationCodeInvalid) {
		t.Fatalf("pseudo reset token did not preserve the generic invalid-code response: %v", err)
	}
}

type flakyCreateStore struct {
	dao.PlatformStore
	fail bool
}

func (s *flakyCreateStore) CreateAccount(ctx context.Context, params domain.CreateAccountParams, guestID *uint64) (domain.Account, domain.GuestImportResult, error) {
	if s.fail {
		s.fail = false
		return domain.Account{}, domain.GuestImportResult{}, errors.New("transient database failure")
	}
	return s.PlatformStore.CreateAccount(ctx, params, guestID)
}

func TestRegistrationReleasesClaimAfterTransientDatabaseFailure(t *testing.T) {
	fixture := newPlatform(t)
	flaky := &flakyCreateStore{PlatformStore: fixture.store, fail: true}
	fixture.platform = service.NewPlatform(
		flaky, fixture.jwt, fixture.verification, fixture.mailer,
		service.WithPlatformClock(func() time.Time { return fixture.now }),
	)
	request, message := fixture.requestRegistration(t)
	if _, err := fixture.platform.Register(context.Background(), request.RegistrationToken, message.Code, nil); err == nil {
		t.Fatal("transient database failure was hidden")
	}
	if _, err := fixture.platform.Register(context.Background(), request.RegistrationToken, message.Code, nil); err != nil {
		t.Fatalf("released challenge could not be retried: %v", err)
	}
}

func TestGuestCredentialIsOpaqueAndOnlyDigestReachesStore(t *testing.T) {
	fixture := newPlatform(t)
	token, guest, err := fixture.platform.CreateGuest(context.Background())
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	if len(token) != 43 || len(guest.TokenHash) != 32 {
		t.Fatalf("unexpected token/hash sizes: token=%d hash=%d", len(token), len(guest.TokenHash))
	}
	if string(guest.TokenHash) == token {
		t.Fatal("plaintext guest token reached durable storage")
	}
	resolved, err := fixture.platform.ResolveGuest(context.Background(), token)
	if err != nil || resolved.ID != guest.ID {
		t.Fatalf("resolve guest: guest=%+v err=%v", resolved, err)
	}
	if _, err := fixture.platform.ResolveGuest(context.Background(), token+"x"); !errors.Is(err, service.ErrGuestUnavailable) {
		t.Fatalf("invalid token accepted: %v", err)
	}
}

func TestGuestSessionLifecycleAndFeatureSwitches(t *testing.T) {
	fixture := newPlatform(t)
	fixture.platform = service.NewPlatform(
		fixture.store, fixture.jwt, fixture.verification, fixture.mailer,
		service.WithPlatformClock(func() time.Time { return fixture.now }),
		service.WithGuestDeepRead(false), service.WithGuestImport(false), service.WithCostBreaker(false),
	)
	token, guest, err := fixture.platform.CreateGuest(context.Background())
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	view := fixture.platform.GuestSession(context.Background(), guest)
	if view.ActorType != service.WebActorGuest || view.Guest == nil || view.Guest.DeepReadState != domain.GuestDeepReadAvailable {
		t.Fatalf("unexpected guest view: %+v", view)
	}
	if view.Features.GuestDeepRead || view.Features.GuestImport || view.Features.CostBreaker {
		t.Fatalf("disabled features leaked as enabled: %+v", view.Features)
	}
	if err := fixture.platform.ClearGuest(context.Background(), token); err != nil {
		t.Fatalf("clear guest: %v", err)
	}
	if _, err := fixture.platform.ResolveGuest(context.Background(), token); !errors.Is(err, service.ErrGuestUnavailable) {
		t.Fatalf("cleared credential remained active: %v", err)
	}
}

func TestGuestAccessSwitchPreventsIssuance(t *testing.T) {
	fixture := newPlatform(t)
	fixture.platform = service.NewPlatform(
		fixture.store, fixture.jwt, fixture.verification, fixture.mailer,
		service.WithGuestAccess(false),
	)
	if _, _, err := fixture.platform.CreateGuest(context.Background()); !errors.Is(err, service.ErrGuestAccessDisabled) {
		t.Fatalf("disabled guest issuance: %v", err)
	}
}

type grandfatherStore struct {
	dao.PlatformStore
	account domain.Account
}

func (s grandfatherStore) GetAccountByEmail(context.Context, string) (domain.Account, error) {
	return s.account, nil
}

func (s grandfatherStore) GetAccountByUsername(context.Context, string) (domain.Account, error) {
	return s.account, nil
}

func TestHistoricalUnverifiedAccountIsGrandfathered(t *testing.T) {
	fixture := newPlatform(t)
	hash, err := auth.HashPassword("hunter2hunter")
	if err != nil {
		t.Fatal(err)
	}
	account := domain.Account{ID: 99, Username: "legacy", Email: "legacy@example.com", Nickname: "Legacy", PasswordHash: hash, Status: domain.AccountStatusActive, AuthVersion: 1}
	platform := service.NewPlatform(grandfatherStore{PlatformStore: fixture.store, account: account}, fixture.jwt, fixture.verification, fixture.mailer)
	login, err := platform.Login(context.Background(), account.Email, "hunter2hunter")
	if err != nil {
		t.Fatalf("grandfathered login rejected: %v", err)
	}
	if _, err := platform.Authenticate(context.Background(), login.Token); err != nil {
		t.Fatalf("grandfathered token rejected: %v", err)
	}
}
