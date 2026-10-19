package router_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"paperassistant/internal/auth"
	"paperassistant/internal/config"
	"paperassistant/internal/controller"
	"paperassistant/internal/dao/daotest"
	"paperassistant/internal/provider/mail"
	"paperassistant/internal/provider/ratelimit"
	"paperassistant/internal/provider/verification"
	"paperassistant/internal/router"
	"paperassistant/internal/service"
)

func limitedServer(t *testing.T, apiPerMinute int, loginLimit int) http.Handler {
	t.Helper()
	limiter := ratelimit.NewMemory(nil)
	verificationStore := verification.NewFake(verification.Config{Pepper: []byte("test-verification-pepper")})
	platformSvc := service.NewPlatform(daotest.NewFakePlatformStore(), auth.NewJWT("test-secret", time.Hour),
		verificationStore, &mail.FakeSender{}, service.WithLoginThrottle(limiter, loginLimit, 100, 15*time.Minute))
	cfg := config.Config{Environment: "development", Platform: config.PlatformConfig{APIPerMinute: apiPerMinute}}
	platform := controller.NewPlatform(platformSvc, cfg).WithAPIRateLimit(limiter)
	return router.New(router.Dependencies{Config: cfg, Platform: platform, Chat: controller.NewChat(nil, platformSvc)})
}

func fromIP(ip string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = ip + ":40000" }
}

func TestAPIBudgetIsPerIPAndSkipsNonAPIPaths(t *testing.T) {
	h := limitedServer(t, 3, 5)
	for i := 0; i < 3; i++ {
		if rec := do(t, h, http.MethodGet, "/api/session", nil, fromIP("198.51.100.1")); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d within budget was limited", i)
		}
	}
	rec := do(t, h, http.MethodGet, "/api/session", nil, fromIP("198.51.100.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over budget: got %d", rec.Code)
	}
	if seconds, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || seconds < 1 || seconds > 60 {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if rec := do(t, h, http.MethodGet, "/api/session", nil, fromIP("198.51.100.2")); rec.Code == http.StatusTooManyRequests {
		t.Fatal("a different IP shared the budget")
	}
	if rec := do(t, h, http.MethodGet, "/", nil, fromIP("198.51.100.1")); rec.Code == http.StatusTooManyRequests {
		t.Fatal("non-API path was limited")
	}
}

func TestLoginThrottleReturns429WithRetryAfter(t *testing.T) {
	h := limitedServer(t, 1000, 2)
	body := map[string]string{"email": "nobody@example.com", "password": "wrong-password"}
	for i := 0; i < 2; i++ {
		if rec := do(t, h, http.MethodPost, "/api/login", body, fromIP("198.51.100.1")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, h, http.MethodPost, "/api/login", body, fromIP("198.51.100.1"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("locked login: got %d retry=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
