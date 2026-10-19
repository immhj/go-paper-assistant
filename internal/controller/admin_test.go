package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"paperassistant/internal/config"
)

func TestRuntimeConfigShowsSafePlatformStateWithoutSecrets(t *testing.T) {
	cfg := config.Config{
		Environment: "production",
		Redis:       config.RedisConfig{Addr: "redis:6379", Password: "redis-secret"},
		SMTP: config.SMTPConfig{
			Mode: "smtp", Host: "smtp.example.com", From: "bot@example.com", AuthCode: "smtp-secret",
		},
		Platform: config.PlatformConfig{
			JWTSecret: "jwt-secret", VerificationPepper: "pepper-secret",
			OpenRegistrationEnabled: true, GuestAccessEnabled: true, GuestDeepReadEnabled: true,
			GuestImportEnabled: true, EmailVerificationRequired: true, CostBreakerEnabled: true,
			CostHourlyUSD: 2, CostDailyUSD: 10,
		},
	}
	rec := httptest.NewRecorder()
	NewAdmin(nil, cfg).RuntimeConfig(rec, httptest.NewRequest("GET", "/api/admin/config", nil))
	body := rec.Body.String()
	for _, secret := range []string{"redis-secret", "smtp-secret", "jwt-secret", "pepper-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("runtime config leaked secret %q: %s", secret, body)
		}
	}
	for _, expected := range []string{`"redisConfigured":true`, `"smtpMode":"smtp"`, `"smtpConfigured":true`, `"costBreaker":true`, `"hourlyUSD":2`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("runtime config missing %s: %s", expected, body)
		}
	}
}
