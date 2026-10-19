package config

import (
	"strings"
	"testing"
	"time"
)

func clearSecurityEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"JWT_SECRET", "VERIFICATION_PEPPER", "SMTP_MODE", "SMTP_HOST", "SMTP_PORT", "SMTP_FROM", "SMTP_AUTH_CODE", "SMTP_TLS_MODE",
		"PLATFORM_OPEN_REGISTRATION_ENABLED", "PLATFORM_GUEST_ACCESS_ENABLED", "PLATFORM_GUEST_DEEP_READ_ENABLED",
		"PLATFORM_GUEST_IMPORT_ENABLED", "PLATFORM_EMAIL_VERIFICATION_REQUIRED", "PLATFORM_COST_BREAKER_ENABLED",
		"REDIS_ADDR", "REDIS_PASSWORD", "REDIS_DB", "TRUSTED_PROXY_CIDRS",
	} {
		t.Setenv(key, "")
	}
}

func TestDevelopmentHasSafeZeroConfigDefaults(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("APP_ENV", "development")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load development defaults: %v", err)
	}
	if cfg.Redis.Addr != "127.0.0.1:6379" || cfg.SMTP.Mode != "log" {
		t.Fatalf("unexpected development dependency defaults: redis=%q smtp=%q", cfg.Redis.Addr, cfg.SMTP.Mode)
	}
	if cfg.Platform.VerificationTTL != 10*time.Minute || cfg.Platform.VerificationMaxAttempts != 5 ||
		cfg.Platform.VerificationResendAfter != time.Minute || cfg.Platform.VerificationEmailHourlyLimit != 5 ||
		cfg.Platform.VerificationIPHourlyLimit != 20 {
		t.Fatalf("unexpected verification defaults: %+v", cfg.Platform)
	}
	if cfg.Platform.CostHourlyUSD != 1 || cfg.Platform.CostDailyUSD != 5 || !cfg.Platform.EmailVerificationRequired || !cfg.Platform.CostBreakerEnabled {
		t.Fatalf("unexpected production-safety defaults: %+v", cfg.Platform)
	}
}

func productionBase(t *testing.T) {
	t.Helper()
	clearSecurityEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("ADMIN_USER", "operator")
	t.Setenv("ADMIN_PASSWORD", "strong-admin-password")
	t.Setenv("SMTP_MODE", "smtp")
	t.Setenv("SMTP_HOST", "smtp.qq.com")
	t.Setenv("SMTP_PORT", "465")
	t.Setenv("SMTP_FROM", "bot@qq.com")
	t.Setenv("SMTP_AUTH_CODE", "qq-smtp-authorization-code")
	t.Setenv("SMTP_TLS_MODE", "implicit")
}

func TestProductionRequiresJWTVerificationPepperAndSMTP(t *testing.T) {
	productionBase(t)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("missing JWT secret: %v", err)
	}
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VERIFICATION_PEPPER") {
		t.Fatalf("missing verification pepper: %v", err)
	}
	t.Setenv("VERIFICATION_PEPPER", "abcdef0123456789abcdef0123456789")
	t.Setenv("SMTP_AUTH_CODE", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SMTP_AUTH_CODE") {
		t.Fatalf("missing SMTP auth code: %v", err)
	}
}

func TestProductionRejectsLogMailerAndUnconfirmedSafetyFlags(t *testing.T) {
	productionBase(t)
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("VERIFICATION_PEPPER", "abcdef0123456789abcdef0123456789")
	t.Setenv("SMTP_MODE", "log")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SMTP_MODE") {
		t.Fatalf("production log mode accepted: %v", err)
	}
	t.Setenv("SMTP_MODE", "smtp")
	t.Setenv("PLATFORM_EMAIL_VERIFICATION_REQUIRED", "false")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "EMAIL_VERIFICATION") {
		t.Fatalf("disabled production email verification accepted: %v", err)
	}
	t.Setenv("PLATFORM_EMAIL_VERIFICATION_REQUIRED", "true")
	t.Setenv("PLATFORM_COST_BREAKER_ENABLED", "false")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COST_BREAKER") {
		t.Fatalf("disabled production cost breaker accepted: %v", err)
	}
}

func TestPlatformFlagTyposFailClosed(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("PLATFORM_GUEST_ACCESS_ENABLED", "maybe")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PLATFORM_GUEST_ACCESS_ENABLED") {
		t.Fatalf("invalid boolean silently accepted: %v", err)
	}
}

func TestUnsetAppEnvDefaultsToProductionAndNeverUsesDevFallbacks(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("APP_ENV", "")
	t.Setenv("ADMIN_USER", "operator")
	t.Setenv("ADMIN_PASSWORD", "strong-admin-password")
	// No JWT secret configured: production validation must reject startup instead of
	// substituting the hard-coded development JWT secret.
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("unset APP_ENV did not fail closed: %v", err)
	}
}

func TestAdminListenerDefaultsToLoopbackAndMustDifferFromPublic(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("APP_ADDR", "")
	t.Setenv("ADMIN_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AdminAddress != "127.0.0.1:9090" {
		t.Fatalf("admin listener default = %q, want loopback", cfg.AdminAddress)
	}
	t.Setenv("ADMIN_ADDR", ":8080")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADMIN_ADDR") {
		t.Fatalf("admin listener sharing the public address accepted: %v", err)
	}
}

func TestUnknownAppEnvIsRejected(t *testing.T) {
	for _, value := range []string{"prod", "staging", "dev"} {
		clearSecurityEnv(t)
		t.Setenv("APP_ENV", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "APP_ENV") {
			t.Fatalf("APP_ENV=%q accepted: %v", value, err)
		}
	}
}

func TestProductionRequiresLongAdminPassword(t *testing.T) {
	productionBase(t)
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("VERIFICATION_PEPPER", "abcdef0123456789abcdef0123456789")
	t.Setenv("ADMIN_PASSWORD", "short-password")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADMIN_PASSWORD") {
		t.Fatalf("short production admin password accepted: %v", err)
	}
	t.Setenv("ADMIN_PASSWORD", "strong-admin-password")
	if _, err := Load(); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
}

func TestUnparseableTypedValuesFailClosed(t *testing.T) {
	cases := map[string]string{
		"JWT_TTL":                   "24 hours",
		"REDIS_DB":                  "zero",
		"PLATFORM_COST_DAILY_USD":   "5usd",
		"PLATFORM_API_PER_MINUTE":   "sixty",
		"QQ_ENABLED":                "yes please",
		"VERIFICATION_RESEND_AFTER": "1 minute",
	}
	for key, value := range cases {
		clearSecurityEnv(t)
		t.Setenv("APP_ENV", "development")
		t.Setenv(key, value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s=%q silently accepted: %v", key, value, err)
		}
		t.Setenv(key, "")
	}
}

func TestTrustedProxyCIDRsAreParsedAndInvalidValuesRejected(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 2001:db8::/32")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load trusted proxies: %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 || cfg.TrustedProxyCIDRs[0].String() != "10.0.0.0/8" || cfg.TrustedProxyCIDRs[1].String() != "2001:db8::/32" {
		t.Fatalf("unexpected trusted proxies: %v", cfg.TrustedProxyCIDRs)
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.1")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("invalid trusted proxy CIDR accepted: %v", err)
	}
}
