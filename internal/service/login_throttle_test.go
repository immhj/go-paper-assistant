package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"paperassistant/internal/provider/ratelimit"
	"paperassistant/internal/service"
)

func throttledPlatform(t *testing.T, fixture *platformFixture, limiter ratelimit.Store, emailLimit, ipLimit int) *service.Platform {
	t.Helper()
	return service.NewPlatform(fixture.store, fixture.jwt, fixture.verification, fixture.mailer,
		service.WithPlatformClock(func() time.Time { return fixture.now }),
		service.WithLoginThrottle(limiter, emailLimit, ipLimit, 15*time.Minute))
}

func login(p *service.Platform, email, password, ip string) error {
	_, err := p.Login(context.Background(), email, password, ip)
	return err
}

func TestLoginLocksEmailAfterFailuresEvenWithCorrectPassword(t *testing.T) {
	fixture := newPlatform(t)
	fixture.register(t)
	now := time.Unix(1_700_000_000, 0)
	limiter := ratelimit.NewMemory(func() time.Time { return now })
	platform := throttledPlatform(t, fixture, limiter, 3, 100)

	// Spread attempts across IPs: the email lock must hold regardless of source address.
	for i, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		if err := login(platform, "alice@example.com", "wrong-password", ip); !errors.Is(err, service.ErrInvalidCredentials) {
			t.Fatalf("failure %d: got %v", i, err)
		}
	}
	err := login(platform, "alice@example.com", "hunter2hunter", "198.51.100.9")
	var throttled *service.ThrottledError
	if !errors.As(err, &throttled) || !errors.Is(err, service.ErrThrottled) || throttled.RetryAfter <= 0 {
		t.Fatalf("locked email accepted correct password: %v", err)
	}

	now = now.Add(16 * time.Minute) // window expired
	if err := login(platform, "alice@example.com", "hunter2hunter", "198.51.100.9"); err != nil {
		t.Fatalf("login after window: %v", err)
	}
}

func TestSuccessfulLoginResetsEmailCounter(t *testing.T) {
	fixture := newPlatform(t)
	fixture.register(t)
	platform := throttledPlatform(t, fixture, ratelimit.NewMemory(nil), 3, 100)
	for round := 0; round < 3; round++ {
		for i := 0; i < 2; i++ {
			if err := login(platform, "alice@example.com", "wrong-password", "198.51.100.1"); !errors.Is(err, service.ErrInvalidCredentials) {
				t.Fatalf("round %d failure %d: %v", round, i, err)
			}
		}
		// 2 failures then a success: the owner typing a password wrong occasionally is never locked.
		if err := login(platform, "alice@example.com", "hunter2hunter", "198.51.100.1"); err != nil {
			t.Fatalf("round %d success: %v", round, err)
		}
	}
}

func TestLoginLocksIPAcrossManyEmailsIncludingUnknownOnes(t *testing.T) {
	fixture := newPlatform(t)
	fixture.register(t)
	platform := throttledPlatform(t, fixture, ratelimit.NewMemory(nil), 3, 5)
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com"} {
		if err := login(platform, email, "whatever-password", "203.0.113.7"); !errors.Is(err, service.ErrInvalidCredentials) {
			t.Fatalf("spray %s: %v", email, err)
		}
	}
	if err := login(platform, "alice@example.com", "hunter2hunter", "203.0.113.7"); !errors.Is(err, service.ErrThrottled) {
		t.Fatalf("sprayed IP not locked: %v", err)
	}
	if err := login(platform, "alice@example.com", "hunter2hunter", "203.0.113.8"); err != nil {
		t.Fatalf("other IP wrongly locked: %v", err)
	}
}

func TestLoginThrottleFailsOpenWhenStoreUnavailable(t *testing.T) {
	fixture := newPlatform(t)
	fixture.register(t)
	limiter := ratelimit.NewMemory(nil)
	limiter.Err = ratelimit.ErrUnavailable
	platform := throttledPlatform(t, fixture, limiter, 1, 1)
	if err := login(platform, "alice@example.com", "hunter2hunter", "198.51.100.1"); err != nil {
		t.Fatalf("login during limiter outage: %v", err)
	}
	if err := login(platform, "alice@example.com", "wrong-password", "198.51.100.1"); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Fatalf("wrong password during outage: %v", err)
	}
}
