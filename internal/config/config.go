package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains process configuration. Secrets are read from environment variables and are
// never persisted by the application.
type Config struct {
	Environment       string
	Address           string
	ShutdownTimeout   time.Duration
	TrustedProxyCIDRs []netip.Prefix
	MySQL             MySQLConfig
	Redis             RedisConfig
	SMTP              SMTPConfig
	Admin             AdminConfig
	Platform          PlatformConfig
	QQ                QQConfig
	PaperScraper      PaperScraperConfig
	DeepSeek          DeepSeekConfig
	FullText          FullTextConfig
}

type MySQLConfig struct {
	DSN            string
	ConnectTimeout time.Duration
}

type RedisConfig struct {
	Addr         string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

type SMTPConfig struct {
	Mode     string
	Host     string
	Port     int
	From     string
	AuthCode string
	TLSMode  string
	Timeout  time.Duration
}

type AdminConfig struct {
	Username string
	Password string
}

// PlatformConfig holds account security, verification, feature rollout and cost-breaker defaults.
type PlatformConfig struct {
	JWTSecret string
	JWTTTL    time.Duration

	VerificationPepper            string
	VerificationTTL               time.Duration
	VerificationMaxAttempts       int
	VerificationResendAfter       time.Duration
	VerificationEmailHourlyLimit  int
	VerificationIPHourlyLimit     int
	VerificationGlobalHourlyLimit int
	VerificationRateWindow        time.Duration

	OpenRegistrationEnabled   bool
	GuestAccessEnabled        bool
	GuestDeepReadEnabled      bool
	GuestImportEnabled        bool
	EmailVerificationRequired bool
	CostBreakerEnabled        bool

	CostHourlyUSD      float64
	CostDailyUSD       float64
	APIPerMinute       int
	LoginFailureLimit  int
	LoginFailureWindow time.Duration
}

type QQConfig struct {
	Enabled      bool
	AppID        string
	AppSecret    string
	APIBase      string
	TokenURL     string
	GatewayURL   string
	Intents      uint32
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

type PaperScraperConfig struct {
	BaseURL     string
	ResultLimit int
	Timeout     time.Duration
}

type DeepSeekConfig struct {
	APIBase          string
	APIKey           string
	Model            string
	Timeout          time.Duration
	PeakInputPerM    float64
	PeakCacheHitPerM float64
	PeakOutputPerM   float64
}

type FullTextConfig struct {
	Directory   string
	PDFToText   string
	MaxPDFBytes int64
}

func Load() (Config, error) {
	for _, key := range []string{
		"PLATFORM_OPEN_REGISTRATION_ENABLED", "PLATFORM_GUEST_ACCESS_ENABLED",
		"PLATFORM_GUEST_DEEP_READ_ENABLED", "PLATFORM_GUEST_IMPORT_ENABLED",
		"PLATFORM_EMAIL_VERIFICATION_REQUIRED", "PLATFORM_COST_BREAKER_ENABLED", "QQ_ENABLED",
	} {
		if err := validateBoolEnv(key); err != nil {
			return Config{}, err
		}
	}

	trustedProxyCIDRs, err := parseTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Environment:       env("APP_ENV", "development"),
		Address:           env("APP_ADDR", ":8080"),
		ShutdownTimeout:   envDuration("SHUTDOWN_TIMEOUT", 15*time.Second),
		TrustedProxyCIDRs: trustedProxyCIDRs,
		MySQL: MySQLConfig{
			DSN:            env("MYSQL_DSN", "paper:paper@tcp(127.0.0.1:3306)/paper_assistant?parseTime=true&charset=utf8mb4&loc=Local"),
			ConnectTimeout: envDuration("MYSQL_CONNECT_TIMEOUT", 60*time.Second),
		},
		Redis: RedisConfig{
			Addr: env("REDIS_ADDR", "127.0.0.1:6379"), Password: os.Getenv("REDIS_PASSWORD"),
			DB: envInt("REDIS_DB", 0), DialTimeout: envDuration("REDIS_DIAL_TIMEOUT", 5*time.Second),
			ReadTimeout: envDuration("REDIS_READ_TIMEOUT", 3*time.Second), WriteTimeout: envDuration("REDIS_WRITE_TIMEOUT", 3*time.Second),
		},
		SMTP: SMTPConfig{
			Mode: strings.ToLower(env("SMTP_MODE", "log")), Host: env("SMTP_HOST", "smtp.qq.com"),
			Port: envInt("SMTP_PORT", 465), From: strings.TrimSpace(os.Getenv("SMTP_FROM")),
			AuthCode: strings.TrimSpace(os.Getenv("SMTP_AUTH_CODE")), TLSMode: strings.ToLower(env("SMTP_TLS_MODE", "implicit")),
			Timeout: envDuration("SMTP_TIMEOUT", 15*time.Second),
		},
		Admin: AdminConfig{Username: env("ADMIN_USER", "admin"), Password: env("ADMIN_PASSWORD", "admin")},
		Platform: PlatformConfig{
			JWTSecret: strings.TrimSpace(os.Getenv("JWT_SECRET")), JWTTTL: envDuration("JWT_TTL", 24*time.Hour),
			VerificationPepper:            strings.TrimSpace(os.Getenv("VERIFICATION_PEPPER")),
			VerificationTTL:               envDuration("VERIFICATION_TTL", 10*time.Minute),
			VerificationMaxAttempts:       envInt("VERIFICATION_MAX_ATTEMPTS", 5),
			VerificationResendAfter:       envDuration("VERIFICATION_RESEND_AFTER", time.Minute),
			VerificationEmailHourlyLimit:  envInt("VERIFICATION_EMAIL_HOURLY_LIMIT", 5),
			VerificationIPHourlyLimit:     envInt("VERIFICATION_IP_HOURLY_LIMIT", 20),
			VerificationGlobalHourlyLimit: envInt("VERIFICATION_GLOBAL_HOURLY_LIMIT", 200),
			VerificationRateWindow:        envDuration("VERIFICATION_RATE_WINDOW", time.Hour),
			OpenRegistrationEnabled:       envBool("PLATFORM_OPEN_REGISTRATION_ENABLED", true),
			GuestAccessEnabled:            envBool("PLATFORM_GUEST_ACCESS_ENABLED", true),
			GuestDeepReadEnabled:          envBool("PLATFORM_GUEST_DEEP_READ_ENABLED", true),
			GuestImportEnabled:            envBool("PLATFORM_GUEST_IMPORT_ENABLED", true),
			EmailVerificationRequired:     envBool("PLATFORM_EMAIL_VERIFICATION_REQUIRED", true),
			CostBreakerEnabled:            envBool("PLATFORM_COST_BREAKER_ENABLED", true),
			CostHourlyUSD:                 envFloat("PLATFORM_COST_HOURLY_USD", 2), CostDailyUSD: envFloat("PLATFORM_COST_DAILY_USD", 10),
			APIPerMinute: envInt("PLATFORM_API_PER_MINUTE", 30), LoginFailureLimit: envInt("PLATFORM_LOGIN_FAILURE_LIMIT", 5),
			LoginFailureWindow: envDuration("PLATFORM_LOGIN_FAILURE_WINDOW", 15*time.Minute),
		},
		QQ: QQConfig{
			Enabled: envBool("QQ_ENABLED", false), AppID: strings.TrimSpace(os.Getenv("QQ_APP_ID")),
			AppSecret: strings.TrimSpace(os.Getenv("QQ_APP_SECRET")), APIBase: strings.TrimRight(env("QQ_API_BASE", "https://api.bot.qq.com"), "/"),
			TokenURL: env("QQ_TOKEN_URL", "https://api.bot.qq.com/app/getAppAccessToken"), GatewayURL: strings.TrimSpace(os.Getenv("QQ_GATEWAY_URL")),
			Intents: uint32(envInt("QQ_INTENTS", 1<<25)), ReconnectMin: envDuration("QQ_RECONNECT_MIN", 2*time.Second),
			ReconnectMax: envDuration("QQ_RECONNECT_MAX", 30*time.Second),
		},
		PaperScraper: PaperScraperConfig{
			BaseURL:     strings.TrimRight(env("PAPERSCRAPER_URL", "http://127.0.0.1:8090"), "/"),
			ResultLimit: envInt("PAPERSCRAPER_RESULT_LIMIT", 5), Timeout: envDuration("PAPERSCRAPER_TIMEOUT", 90*time.Second),
		},
		DeepSeek: DeepSeekConfig{
			APIBase: strings.TrimRight(env("DEEPSEEK_API_BASE", "https://api.deepseek.com"), "/"), APIKey: strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")),
			Model: env("DEEPSEEK_MODEL", "deepseek-flash"), Timeout: envDuration("DEEPSEEK_TIMEOUT", 180*time.Second),
			PeakInputPerM: envFloat("DEEPSEEK_PEAK_INPUT_PER_M", 0.30), PeakCacheHitPerM: envFloat("DEEPSEEK_PEAK_CACHE_HIT_PER_M", 0.006),
			PeakOutputPerM: envFloat("DEEPSEEK_PEAK_OUTPUT_PER_M", 1.20),
		},
		FullText: FullTextConfig{
			Directory: env("FULLTEXT_DIR", "data/fulltext"), PDFToText: env("PDFTOTEXT_BIN", "pdftotext"),
			MaxPDFBytes: int64(envInt("MAX_PDF_BYTES", 50*1024*1024)),
		},
	}

	if cfg.Environment == "development" {
		if cfg.Platform.JWTSecret == "" {
			cfg.Platform.JWTSecret = "dev-insecure-jwt-secret-change-me"
		}
		if cfg.Platform.VerificationPepper == "" {
			cfg.Platform.VerificationPepper = "dev-insecure-verification-pepper-change-me"
		}
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg Config) error {
	if cfg.MySQL.DSN == "" {
		return errors.New("MYSQL_DSN is required")
	}
	if cfg.Redis.Addr == "" || cfg.Redis.DB < 0 || cfg.Redis.DialTimeout <= 0 || cfg.Redis.ReadTimeout <= 0 || cfg.Redis.WriteTimeout <= 0 {
		return errors.New("Redis configuration is invalid")
	}
	if cfg.QQ.Enabled && (cfg.QQ.AppID == "" || cfg.QQ.AppSecret == "") {
		return errors.New("QQ_APP_ID and QQ_APP_SECRET are required when QQ_ENABLED=true")
	}
	if cfg.PaperScraper.ResultLimit < 1 || cfg.PaperScraper.ResultLimit > 100 {
		return fmt.Errorf("PAPERSCRAPER_RESULT_LIMIT must be between 1 and 100, got %d", cfg.PaperScraper.ResultLimit)
	}
	if cfg.QQ.ReconnectMin <= 0 || cfg.QQ.ReconnectMax < cfg.QQ.ReconnectMin {
		return errors.New("QQ reconnect durations are invalid")
	}
	if cfg.Environment != "development" && cfg.Admin.Username == "admin" && cfg.Admin.Password == "admin" {
		return errors.New("ADMIN_USER/ADMIN_PASSWORD defaults are forbidden outside development")
	}
	if cfg.Platform.JWTSecret == "" {
		return errors.New("JWT_SECRET is required outside development")
	}
	if cfg.Platform.JWTTTL <= 0 {
		return errors.New("JWT_TTL must be positive")
	}
	if len(cfg.Platform.VerificationPepper) < 32 {
		return errors.New("VERIFICATION_PEPPER must be at least 32 characters")
	}
	if cfg.Platform.VerificationTTL <= 0 || cfg.Platform.VerificationTTL > 10*time.Minute ||
		cfg.Platform.VerificationMaxAttempts < 1 || cfg.Platform.VerificationMaxAttempts > 5 ||
		cfg.Platform.VerificationResendAfter < time.Minute || cfg.Platform.VerificationRateWindow < time.Hour ||
		cfg.Platform.VerificationEmailHourlyLimit < 1 || cfg.Platform.VerificationEmailHourlyLimit > 5 ||
		cfg.Platform.VerificationIPHourlyLimit < 1 || cfg.Platform.VerificationIPHourlyLimit > 20 ||
		cfg.Platform.VerificationGlobalHourlyLimit < 1 {
		return errors.New("verification security limits are invalid or weaker than the required defaults")
	}
	if cfg.Platform.CostHourlyUSD <= 0 || cfg.Platform.CostDailyUSD <= 0 || cfg.Platform.APIPerMinute < 1 ||
		cfg.Platform.LoginFailureLimit < 1 || cfg.Platform.LoginFailureWindow <= 0 {
		return errors.New("platform cost or security limits are invalid")
	}
	if cfg.SMTP.Mode != "log" && cfg.SMTP.Mode != "smtp" {
		return errors.New("SMTP_MODE must be log or smtp")
	}
	if cfg.SMTP.TLSMode != "implicit" && cfg.SMTP.TLSMode != "starttls" {
		return errors.New("SMTP_TLS_MODE must be implicit or starttls")
	}
	if cfg.SMTP.Timeout <= 0 || cfg.SMTP.Port < 1 || cfg.SMTP.Port > 65535 || cfg.SMTP.Host == "" {
		return errors.New("SMTP host, port and timeout are invalid")
	}
	if cfg.Environment == "production" {
		for _, key := range []string{"SMTP_MODE", "SMTP_HOST", "SMTP_PORT", "SMTP_FROM", "SMTP_AUTH_CODE", "SMTP_TLS_MODE"} {
			if strings.TrimSpace(os.Getenv(key)) == "" {
				return fmt.Errorf("%s is required in production", key)
			}
		}
		if len(cfg.Platform.JWTSecret) < 32 {
			return errors.New("JWT_SECRET must be at least 32 characters in production")
		}
		if cfg.SMTP.Mode != "smtp" {
			return errors.New("SMTP_MODE must be smtp in production")
		}
		if cfg.SMTP.From == "" || cfg.SMTP.AuthCode == "" {
			return errors.New("SMTP_FROM and SMTP_AUTH_CODE are required in production")
		}
		if !cfg.Platform.EmailVerificationRequired {
			return errors.New("PLATFORM_EMAIL_VERIFICATION_REQUIRED must be true in production")
		}
		if !cfg.Platform.CostBreakerEnabled {
			return errors.New("PLATFORM_COST_BREAKER_ENABLED must be true in production")
		}
	}
	if cfg.Environment != "development" && cfg.SMTP.Mode == "log" {
		return errors.New("SMTP_MODE=log is allowed only in development")
	}
	return nil
}

func validateBoolEnv(key string) error {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}
	if _, err := strconv.ParseBool(value); err != nil {
		return fmt.Errorf("%s must be a boolean", key)
	}
	return nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseTrustedProxyCIDRs(value string) ([]netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS contains invalid CIDR %q", part)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}
