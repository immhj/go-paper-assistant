package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"paperassistant/internal/auth"
	"paperassistant/internal/config"
	"paperassistant/internal/controller"
	"paperassistant/internal/dao/daotest"
	"paperassistant/internal/provider/mail"
	"paperassistant/internal/provider/verification"
	"paperassistant/internal/router"
	"paperassistant/internal/service"
)

type testServer struct {
	h            http.Handler
	store        *daotest.FakePlatformStore
	platform     *service.Platform
	verification *verification.Fake
	mailer       *mail.FakeSender
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	store := daotest.NewFakePlatformStore()
	jwt := auth.NewJWT("test-secret", time.Hour)
	verificationStore := verification.NewFake(verification.Config{
		Pepper: []byte("test-verification-pepper"), TTL: 10 * time.Minute, MaxAttempts: 5,
		ResendAfter: time.Minute, EmailHourlyLimit: 5, IPHourlyLimit: 20, GlobalHourlyLimit: 200,
	})
	mailer := &mail.FakeSender{}
	platformSvc := service.NewPlatform(store, jwt, verificationStore, mailer)
	cfg := config.Config{
		Environment: "development",
		Admin:       config.AdminConfig{Username: "admin", Password: "admin"},
	}
	platformController := controller.NewPlatform(platformSvc, cfg)
	return &testServer{
		h: router.New(router.Dependencies{
			Config: cfg, Platform: platformController, Chat: controller.NewChat(nil, platformSvc),
		}),
		store: store, platform: platformSvc, verification: verificationStore, mailer: mailer,
	}
}

func do(t *testing.T, h http.Handler, method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cookieByName(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("response missing %s cookie: %v", name, rec.Header().Values("Set-Cookie"))
	return nil
}

func requestCode(t *testing.T, fixture *testServer, purpose, email string) (string, string) {
	t.Helper()
	path := "/api/register/code"
	body := any(map[string]string{"email": email, "nickname": "Tester", "password": "hunter2hunter"})
	if purpose == "reset" {
		path = "/api/password-reset/code"
		body = map[string]string{"email": email}
	}
	rec := do(t, fixture.h, http.MethodPost, path, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request %s code: got %d body=%s", purpose, rec.Code, rec.Body.String())
	}
	var response struct {
		RegistrationToken string `json:"registrationToken"`
		ResetToken        string `json:"resetToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode code response: %v", err)
	}
	message, ok := fixture.mailer.Last()
	if !ok || message.Code == "" {
		t.Fatal("verification code was not delivered to fake mailer")
	}
	if purpose == "reset" {
		return response.ResetToken, message.Code
	}
	return response.RegistrationToken, message.Code
}

func register(t *testing.T, fixture *testServer, email string, cookie *http.Cookie) string {
	t.Helper()
	token, code := requestCode(t, fixture, "registration", email)
	rec := do(t, fixture.h, http.MethodPost, "/api/register", map[string]string{
		"registrationToken": token, "code": code,
	}, func(r *http.Request) {
		if cookie != nil {
			r.AddCookie(cookie)
		}
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Token == "" {
		t.Fatalf("register response: err=%v body=%s", err, rec.Body.String())
	}
	return response.Token
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func TestSessionCreatesGuestCookieWithRequiredAttributes(t *testing.T) {
	fixture := newTestServer(t)
	rec := do(t, fixture.h, http.MethodGet, "/api/session", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"actorType":"guest"`) {
		t.Fatalf("guest session: got %d body=%s", rec.Code, rec.Body.String())
	}
	cookie := cookieByName(t, rec, "guest_session")
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge != 30*24*60*60 || cookie.Secure {
		t.Fatalf("unexpected guest cookie attributes: %+v", cookie)
	}
	if _, err := fixture.platform.ResolveGuest(context.Background(), cookie.Value); err != nil {
		t.Fatalf("issued guest cookie is not resolvable: %v", err)
	}
}

func TestSessionUsesAccountAndInvalidBearerNeverFallsBackToGuest(t *testing.T) {
	fixture := newTestServer(t)
	accountToken := register(t, fixture, "account@example.com", nil)

	rec := do(t, fixture.h, http.MethodGet, "/api/session", nil, bearer(accountToken))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"actorType":"account"`) {
		t.Fatalf("account session: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, fixture.h, http.MethodGet, "/api/session", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Basic forged")
	})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"status":"INVALID_TOKEN"`) {
		t.Fatalf("invalid authorization: got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("invalid bearer unexpectedly received guest cookie: %v", rec.Header().Values("Set-Cookie"))
	}
}

func TestRegistrationUpgradesValidGuestAndClearsCookie(t *testing.T) {
	fixture := newTestServer(t)
	session := do(t, fixture.h, http.MethodGet, "/api/session", nil, nil)
	guestCookie := cookieByName(t, session, "guest_session")
	guest, err := fixture.platform.ResolveGuest(context.Background(), guestCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	current := uint64(12)
	if err = fixture.store.SeedGuestProjects(guest.ID, []uint64{11, 12}, &current); err != nil {
		t.Fatal(err)
	}

	registrationToken, code := requestCode(t, fixture, "registration", "upgrade@example.com")
	rec := do(t, fixture.h, http.MethodPost, "/api/register", map[string]string{
		"registrationToken": registrationToken, "code": code,
	}, func(r *http.Request) { r.AddCookie(guestCookie) })
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"upgraded":true`) {
		t.Fatalf("guest upgrade: got %d body=%s", rec.Code, rec.Body.String())
	}
	if !hasClearedGuestCookie(rec) {
		t.Fatalf("registration did not clear guest cookie: %v", rec.Header().Values("Set-Cookie"))
	}
	if _, err = fixture.platform.ResolveGuest(context.Background(), guestCookie.Value); err == nil {
		t.Fatal("upgraded guest credential remained active")
	}
}

func TestLoginReportsGuestImportAvailability(t *testing.T) {
	fixture := newTestServer(t)
	register(t, fixture, "login@example.com", nil)
	session := do(t, fixture.h, http.MethodGet, "/api/session", nil, nil)
	guestCookie := cookieByName(t, session, "guest_session")
	guest, err := fixture.platform.ResolveGuest(context.Background(), guestCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	current := uint64(21)
	if err = fixture.store.SeedGuestProjects(guest.ID, []uint64{21}, &current); err != nil {
		t.Fatal(err)
	}
	rec := do(t, fixture.h, http.MethodPost, "/api/login", map[string]string{
		"email": "login@example.com", "password": "hunter2hunter",
	}, func(r *http.Request) { r.AddCookie(guestCookie) })
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"guestImportAvailable":true`) {
		t.Fatalf("login import availability: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPasswordResetRevokesExistingJWT(t *testing.T) {
	fixture := newTestServer(t)
	oldToken := register(t, fixture, "reset@example.com", nil)
	resetToken, resetCode := requestCode(t, fixture, "reset", "reset@example.com")
	rec := do(t, fixture.h, http.MethodPost, "/api/password-reset", map[string]string{
		"resetToken": resetToken, "code": resetCode, "newPassword": "new-password-123",
	}, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("password reset: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, fixture.h, http.MethodGet, "/api/me", nil, bearer(oldToken))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"status":"INVALID_TOKEN"`) {
		t.Fatalf("old token after reset: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestImportRequiresConfirmationAndIdempotencyKeyThenClearsGuest(t *testing.T) {
	fixture := newTestServer(t)
	accountToken := register(t, fixture, "import@example.com", nil)
	session := do(t, fixture.h, http.MethodGet, "/api/session", nil, nil)
	guestCookie := cookieByName(t, session, "guest_session")
	guest, err := fixture.platform.ResolveGuest(context.Background(), guestCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.store.SeedGuestProjects(guest.ID, []uint64{31}, nil); err != nil {
		t.Fatal(err)
	}

	rec := do(t, fixture.h, http.MethodPost, "/api/guest/import", map[string]bool{"confirm": false}, func(r *http.Request) {
		bearer(accountToken)(r)
		r.Header.Set("Origin", "http://example.com")
		r.AddCookie(guestCookie)
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "CONFIRMATION_REQUIRED") {
		t.Fatalf("missing confirmation: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, fixture.h, http.MethodPost, "/api/guest/import", map[string]bool{"confirm": true}, func(r *http.Request) {
		bearer(accountToken)(r)
		r.Header.Set("Origin", "http://example.com")
		r.AddCookie(guestCookie)
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "IDEMPOTENCY_KEY_REQUIRED") {
		t.Fatalf("missing idempotency key: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, fixture.h, http.MethodPost, "/api/guest/import", map[string]bool{"confirm": true}, func(r *http.Request) {
		bearer(accountToken)(r)
		r.Header.Set("Origin", "http://example.com")
		r.AddCookie(guestCookie)
		r.Header.Set("Idempotency-Key", "import-1")
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"importedProjectNos":[31]`) || !hasClearedGuestCookie(rec) {
		t.Fatalf("guest import: got %d body=%s cookies=%v", rec.Code, rec.Body.String(), rec.Header().Values("Set-Cookie"))
	}
}

func TestDeleteGuestRequiresSameOrigin(t *testing.T) {
	fixture := newTestServer(t)
	session := do(t, fixture.h, http.MethodGet, "/api/session", nil, nil)
	guestCookie := cookieByName(t, session, "guest_session")

	rec := do(t, fixture.h, http.MethodDelete, "/api/guest", nil, func(r *http.Request) {
		r.AddCookie(guestCookie)
		r.Header.Set("Origin", "https://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin delete: got %d", rec.Code)
	}
	rec = do(t, fixture.h, http.MethodDelete, "/api/guest", nil, func(r *http.Request) {
		r.AddCookie(guestCookie)
		r.Header.Set("Origin", "http://example.com")
	})
	if rec.Code != http.StatusNoContent || !hasClearedGuestCookie(rec) {
		t.Fatalf("same-origin delete: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemovedInvitesAre404AndAdminGuestsUsesBasicAuth(t *testing.T) {
	fixture := newTestServer(t)
	admin := func(r *http.Request) { r.SetBasicAuth("admin", "admin") }
	if rec := do(t, fixture.h, http.MethodGet, "/api/admin/invites", nil, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("removed invites endpoint: got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, fixture.h, http.MethodGet, "/api/admin/guests", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin guests without basic auth: got %d", rec.Code)
	}
	rec := do(t, fixture.h, http.MethodGet, "/api/admin/guests", nil, admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":0`) {
		t.Fatalf("admin guests: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRootRedirectsToApp(t *testing.T) {
	fixture := newTestServer(t)
	rec := do(t, fixture.h, http.MethodGet, "/", nil, nil)
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/app/" {
		t.Fatalf("root redirect: got %d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func hasClearedGuestCookie(rec *httptest.ResponseRecorder) bool {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "guest_session" && cookie.MaxAge < 0 {
			return true
		}
	}
	return false
}

func TestVerificationRateLimitIncludesRetryAfter(t *testing.T) {
	fixture := newTestServer(t)
	requestCode(t, fixture, "registration", "cooldown@example.com")
	rec := do(t, fixture.h, http.MethodPost, "/api/register/code", map[string]string{
		"email": "cooldown@example.com", "nickname": "Tester", "password": "hunter2hunter",
	}, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"status":"RATE_LIMITED"`) {
		t.Fatalf("verification cooldown: got %d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
