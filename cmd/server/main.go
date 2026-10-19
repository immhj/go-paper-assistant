package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"paperassistant/internal/auth"
	"paperassistant/internal/config"
	"paperassistant/internal/controller"
	mysqlstore "paperassistant/internal/dao/mysql"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
	"paperassistant/internal/provider/fulltext"
	"paperassistant/internal/provider/guard"
	mailprovider "paperassistant/internal/provider/mail"
	"paperassistant/internal/provider/paperscraper"
	"paperassistant/internal/provider/ratelimit"
	"paperassistant/internal/provider/verification"
	"paperassistant/internal/router"
	"paperassistant/internal/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// JSON until config is loaded so even a config error is a parseable log line.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	cfg, err := config.Load()
	if err != nil {
		fatal("load config", err)
	}
	if cfg.LogFormat == "text" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}
	store, err := mysqlstore.Open(ctx, cfg.MySQL)
	if err != nil {
		fatal("connect mysql", err)
	}
	defer store.Close()

	redisClient := redis.NewClient(&redis.Options{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB,
		DialTimeout: cfg.Redis.DialTimeout, ReadTimeout: cfg.Redis.ReadTimeout, WriteTimeout: cfg.Redis.WriteTimeout,
	})
	defer redisClient.Close()
	if err = redisClient.Ping(ctx).Err(); err != nil {
		fatal("connect redis", err)
	}

	verificationStore, err := verification.NewRedis(redisClient, verification.Config{
		Pepper: []byte(cfg.Platform.VerificationPepper), TTL: cfg.Platform.VerificationTTL,
		MaxAttempts: cfg.Platform.VerificationMaxAttempts, ResendAfter: cfg.Platform.VerificationResendAfter,
		EmailHourlyLimit: cfg.Platform.VerificationEmailHourlyLimit, IPHourlyLimit: cfg.Platform.VerificationIPHourlyLimit,
		GlobalHourlyLimit: cfg.Platform.VerificationGlobalHourlyLimit, RateWindow: cfg.Platform.VerificationRateWindow,
	})
	if err != nil {
		fatal("configure verification store", err)
	}
	mailer, err := mailprovider.NewSender(mailprovider.Config{
		Environment: cfg.Environment, Mode: cfg.SMTP.Mode, Host: cfg.SMTP.Host, Port: cfg.SMTP.Port,
		From: cfg.SMTP.From, AuthCode: cfg.SMTP.AuthCode, TLSMode: cfg.SMTP.TLSMode, Timeout: cfg.SMTP.Timeout,
	}, slog.NewLogLogger(slog.Default().With("component", "mail").Handler(), slog.LevelInfo))
	if err != nil {
		fatal("configure mail sender", err)
	}
	costHourly, costDaily := cfg.Platform.CostHourlyUSD, cfg.Platform.CostDailyUSD
	if !cfg.Platform.CostBreakerEnabled {
		costHourly, costDaily = math.MaxFloat64, math.MaxFloat64
	}
	webGuard, err := guard.NewRedis(redisClient, guard.Config{CostHourlyUSD: costHourly, CostDailyUSD: costDaily})
	if err != nil {
		fatal("configure web guard", err)
	}

	rateLimits := ratelimit.NewRedis(redisClient)
	jwtIssuer := auth.NewJWT(cfg.Platform.JWTSecret, cfg.Platform.JWTTTL)
	platformService := service.NewPlatform(store, jwtIssuer, verificationStore, mailer,
		service.WithLoginThrottle(rateLimits, cfg.Platform.LoginFailureLimit, cfg.Platform.LoginIPFailureLimit, cfg.Platform.LoginFailureWindow),
		service.WithOpenRegistration(cfg.Platform.OpenRegistrationEnabled),
		service.WithGuestAccess(cfg.Platform.GuestAccessEnabled),
		service.WithGuestDeepRead(cfg.Platform.GuestDeepReadEnabled),
		service.WithGuestImport(cfg.Platform.GuestImportEnabled),
		service.WithEmailVerificationRequired(cfg.Platform.EmailVerificationRequired),
		service.WithCostBreaker(cfg.Platform.CostBreakerEnabled),
	)
	qqClient := qq.New(cfg.QQ)
	application := service.New(
		store, paperscraper.New(cfg.PaperScraper), deepseek.New(cfg.DeepSeek), fulltext.New(cfg.FullText), qqClient,
		service.WithPlatformStore(store), service.WithWebGuard(webGuard),
		service.WithGuestDeepReadEnabled(cfg.Platform.GuestDeepReadEnabled),
	)
	adminService := service.NewAdmin(store, application.EmergencyStop())
	platformController := controller.NewPlatform(platformService, cfg).WithAPIRateLimit(rateLimits)
	deps := router.Dependencies{
		Config: cfg, Health: controller.NewHealth(adminService), Admin: controller.NewAdmin(adminService, cfg),
		Platform: platformController, Chat: controller.NewChat(application, platformService).BindPlatformCSRF(platformController),
		Projects: controller.NewProjects(application, platformController),
		Papers:   controller.NewPapers(application, platformController, cfg.FullText.MaxPDFBytes),
	}
	// No WriteTimeout on the public server: SSE chat responses legitimately stream for minutes.
	// ReadTimeout bounds the whole request including a 50 MiB PDF upload body; IdleTimeout
	// reclaims keep-alive connections held open by idle or slow clients.
	server := &http.Server{
		Addr: cfg.Address, Handler: router.New(deps),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}
	adminServer := &http.Server{
		Addr: cfg.AdminAddress, Handler: router.NewAdmin(deps),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
	}

	cleanupExpiredGuests(ctx, platformService)
	go runGuestCleanup(ctx, platformService)

	serverErrors := make(chan error, 2)
	go func() {
		slog.Info("HTTP server started", "addr", cfg.Address, "environment", cfg.Environment)
		serverErrors <- server.ListenAndServe()
	}()
	go func() {
		slog.Info("admin server started", "addr", cfg.AdminAddress)
		serverErrors <- adminServer.ListenAndServe()
	}()
	if cfg.QQ.Enabled {
		go func() {
			slog.Info("QQ gateway starting", "intents", cfg.QQ.Intents)
			if gatewayErr := qqClient.RunGateway(ctx, application.HandleQQ); gatewayErr != nil && !errors.Is(gatewayErr, context.Canceled) {
				slog.Error("QQ gateway stopped", "error", gatewayErr)
			}
		}()
	} else {
		slog.Warn("QQ gateway disabled; set QQ_ENABLED=true after configuring credentials")
	}

	select {
	case <-ctx.Done():
	case err = <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			fatal("HTTP server", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP shutdown", "error", err)
	}
	if err = adminServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("admin HTTP shutdown", "error", err)
	}
	if err = application.Wait(shutdownCtx); err != nil {
		slog.Error("QQ worker shutdown", "error", err)
	}
	slog.Info("server stopped")
}

func runGuestCleanup(ctx context.Context, platform *service.Platform) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupExpiredGuests(ctx, platform)
		}
	}
}

func cleanupExpiredGuests(ctx context.Context, platform *service.Platform) {
	var total domain.GuestMetrics
	for {
		metrics, err := platform.CleanupExpiredGuests(ctx, 100)
		if err != nil {
			total.Failures++
			slog.Error("guest cleanup failed", "scanned", total.Scanned, "deleted_sessions", total.DeletedSessions,
				"deleted_projects", total.DeletedProjects, "skipped_reserved", total.SkippedReserved, "failures", total.Failures)
			return
		}
		total.Scanned += metrics.Scanned
		total.DeletedSessions += metrics.DeletedSessions
		total.DeletedProjects += metrics.DeletedProjects
		total.SkippedReserved += metrics.SkippedReserved
		total.Failures += metrics.Failures
		if metrics.Scanned < 100 {
			break
		}
	}
	slog.Info("guest cleanup completed", "scanned", total.Scanned, "deleted_sessions", total.DeletedSessions,
		"deleted_projects", total.DeletedProjects, "skipped_reserved", total.SkippedReserved, "failures", total.Failures)
}

func fatal(message string, err error) {
	slog.Error(message, "error", err)
	os.Exit(1)
}
