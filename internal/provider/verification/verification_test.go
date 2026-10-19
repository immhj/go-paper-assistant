package verification

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

func testFake(now *time.Time) *Fake {
	return NewFake(Config{
		Pepper: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return *now },
	})
}

func startRegistration(t *testing.T, store *Fake, email, ip string) Challenge {
	t.Helper()
	challenge, err := store.StartRegistration(context.Background(), RegistrationData{
		Email: email, Nickname: "Alice", PasswordHash: "$argon2id$test",
	}, ip)
	if err != nil {
		t.Fatalf("start registration: %v", err)
	}
	if err := store.Activate(context.Background(), PurposeRegistration, challenge.Token); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return challenge
}

func TestChallengeUsesURLSafe32ByteTokenAndSixDigitCode(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := testFake(&now)
	pattern := regexp.MustCompile(`^[0-9]{6}$`)
	for i := 0; i < 100; i++ {
		challenge, err := store.StartRegistration(context.Background(), RegistrationData{Email: "a@example.com"}, "192.0.2.1")
		if err != nil {
			if !errors.Is(err, ErrResendTooSoon) {
				t.Fatalf("start: %v", err)
			}
			now = now.Add(time.Minute)
			continue
		}
		if len(challenge.Token) != 43 {
			t.Fatalf("token length = %d, want 43", len(challenge.Token))
		}
		if !pattern.MatchString(challenge.Code) {
			t.Fatalf("code %q is not fixed six digits", challenge.Code)
		}
		entry := store.entries[store.entryKey(PurposeRegistration, challenge.Token)]
		if entry.codeDigest == challenge.Code {
			t.Fatal("fake stored plaintext code")
		}
		if err := store.Cancel(context.Background(), PurposeRegistration, challenge.Token); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}
}

func TestChallengeExpiresAfterTenMinutes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := testFake(&now)
	challenge := startRegistration(t, store, "a@example.com", "192.0.2.1")
	if got := challenge.ExpiresAt.Sub(now); got != 10*time.Minute {
		t.Fatalf("TTL = %v", got)
	}
	now = now.Add(10 * time.Minute)
	if _, err := store.VerifyRegistration(context.Background(), challenge.Token, challenge.Code); !errors.Is(err, ErrExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestFifthWrongCodeExhaustsChallenge(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := testFake(&now)
	challenge := startRegistration(t, store, "a@example.com", "192.0.2.1")
	for attempt := 1; attempt <= 4; attempt++ {
		if _, err := store.VerifyRegistration(context.Background(), challenge.Token, "999999"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d: want invalid, got %v", attempt, err)
		}
	}
	if _, err := store.VerifyRegistration(context.Background(), challenge.Token, "999999"); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("fifth attempt: want exhausted, got %v", err)
	}
	if _, err := store.VerifyRegistration(context.Background(), challenge.Token, challenge.Code); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("correct code after exhaustion: got %v", err)
	}
}

func TestPurposeIsolationAndAtomicClaim(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := testFake(&now)
	registration := startRegistration(t, store, "a@example.com", "192.0.2.1")
	if _, err := store.VerifyReset(context.Background(), registration.Token, registration.Code); !errors.Is(err, ErrExpired) {
		t.Fatalf("registration code crossed purpose: %v", err)
	}
	claim, err := store.VerifyRegistration(context.Background(), registration.Token, registration.Code)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := store.VerifyRegistration(context.Background(), registration.Token, registration.Code); !errors.Is(err, ErrClaimed) {
		t.Fatalf("concurrent second claim: %v", err)
	}
	if err := store.Release(context.Background(), claim); err != nil {
		t.Fatalf("release: %v", err)
	}
	claim, err = store.VerifyRegistration(context.Background(), registration.Token, registration.Code)
	if err != nil {
		t.Fatalf("verify after release: %v", err)
	}
	if err := store.Consume(context.Background(), claim); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := store.VerifyRegistration(context.Background(), registration.Token, registration.Code); !errors.Is(err, ErrExpired) {
		t.Fatalf("consumed challenge replayed: %v", err)
	}
}

func TestResendCooldownAndCancelPreservesPreviousGeneration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := testFake(&now)
	old := startRegistration(t, store, "a@example.com", "192.0.2.1")
	if _, err := store.StartRegistration(context.Background(), RegistrationData{Email: "a@example.com"}, "192.0.2.1"); !errors.Is(err, ErrResendTooSoon) {
		t.Fatalf("want cooldown, got %v", err)
	}
	now = now.Add(time.Minute)
	pending, err := store.StartRegistration(context.Background(), RegistrationData{Email: "a@example.com"}, "192.0.2.1")
	if err != nil {
		t.Fatalf("resend start: %v", err)
	}
	if err := store.Cancel(context.Background(), PurposeRegistration, pending.Token); err != nil {
		t.Fatalf("cancel pending resend: %v", err)
	}
	if _, err := store.VerifyRegistration(context.Background(), old.Token, old.Code); err != nil {
		t.Fatalf("failed delivery invalidated previous code: %v", err)
	}
	if _, err := store.StartRegistration(context.Background(), RegistrationData{Email: "a@example.com"}, "192.0.2.1"); err != nil {
		t.Fatalf("cancel did not clear cooldown/rate reservation: %v", err)
	}
}

func TestEmailAndIPHourlyLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := NewFake(Config{
		Pepper: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return now },
		EmailHourlyLimit: 2, IPHourlyLimit: 2,
	})
	for i := 0; i < 2; i++ {
		challenge := startRegistration(t, store, "a@example.com", "192.0.2.1")
		_ = challenge
		now = now.Add(time.Minute)
	}
	if _, err := store.StartRegistration(context.Background(), RegistrationData{Email: "a@example.com"}, "192.0.2.2"); !errors.Is(err, ErrEmailRateLimited) {
		t.Fatalf("want email hourly limit, got %v", err)
	}
	if _, err := store.StartRegistration(context.Background(), RegistrationData{Email: "other@example.com"}, "192.0.2.1"); !errors.Is(err, ErrIPRateLimited) {
		t.Fatalf("want IP hourly limit, got %v", err)
	}
}

func TestGlobalHourlyLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := NewFake(Config{
		Pepper: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return now },
		GlobalHourlyLimit: 2,
	})
	for i, email := range []string{"a@example.com", "b@example.com"} {
		if _, err := store.StartRegistration(context.Background(), RegistrationData{Email: email}, string(rune('a'+i))); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, err := store.StartRegistration(context.Background(), RegistrationData{Email: "c@example.com"}, "c"); !errors.Is(err, ErrGlobalRateLimited) {
		t.Fatalf("want global hourly limit, got %v", err)
	}
}
