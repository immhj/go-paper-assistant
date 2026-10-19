package verification

import (
	"context"
	"crypto/hmac"
	"sync"
	"time"
)

// Fake is an in-memory, concurrency-safe Store for service tests. It mirrors the Redis state
// transitions and stores only code digests, never plaintext codes.
type Fake struct {
	mu         sync.Mutex
	cfg        Config
	entries    map[string]*fakeEntry
	active     map[string]string
	cooldowns  map[string]fakeCooldown
	emailRates map[string]*fakeWindow
	ipRates    map[string]*fakeWindow
}

type fakeEntry struct {
	purpose        Purpose
	token          string
	subjectKey     string
	rateSubjectKey string
	rateIPKey      string
	codeDigest     string
	expiresAt      time.Time
	state          string
	attempts       int
	leaseID        string
	registration   RegistrationData
	reset          ResetData
}

type fakeCooldown struct {
	token     string
	expiresAt time.Time
}

type fakeWindow struct {
	expiresAt time.Time
	tokens    map[string]struct{}
}

func NewFake(cfg Config) *Fake {
	cfg = cfg.normalized()
	if len(cfg.Pepper) == 0 {
		cfg.Pepper = []byte("fake-verification-pepper")
	}
	return &Fake{
		cfg: cfg, entries: make(map[string]*fakeEntry), active: make(map[string]string),
		cooldowns: make(map[string]fakeCooldown), emailRates: make(map[string]*fakeWindow),
		ipRates: make(map[string]*fakeWindow),
	}
}

func (f *Fake) StartRegistration(_ context.Context, data RegistrationData, clientIP string) (Challenge, error) {
	return f.start(PurposeRegistration, data.Email, clientIP, data, ResetData{})
}

func (f *Fake) StartReset(_ context.Context, data ResetData, clientIP string) (Challenge, error) {
	return f.start(PurposeReset, data.Email, clientIP, RegistrationData{}, data)
}

func (f *Fake) start(purpose Purpose, subject, clientIP string, registration RegistrationData, reset ResetData) (Challenge, error) {
	challenge, err := newChallenge(f.cfg, purpose)
	if err != nil {
		return Challenge{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.cfg.Now()
	f.cleanup(now)
	subjectKey := string(purpose) + ":" + subjectDigest(f.cfg.Pepper, purpose, subject)
	rateSubjectKey := "email:" + rateSubjectDigest(f.cfg.Pepper, subject)
	rateIPKey := "ip:" + rateSubjectDigest(f.cfg.Pepper, clientIP)
	globalKey := "global"
	if cooldown, ok := f.cooldowns[subjectKey]; ok && now.Before(cooldown.expiresAt) {
		return Challenge{}, ErrResendTooSoon
	}
	if f.windowCount(f.emailRates, rateSubjectKey, now) >= f.cfg.EmailHourlyLimit {
		return Challenge{}, ErrEmailRateLimited
	}
	if f.windowCount(f.ipRates, rateIPKey, now) >= f.cfg.IPHourlyLimit {
		return Challenge{}, ErrIPRateLimited
	}
	if f.windowCount(f.emailRates, globalKey, now) >= f.cfg.GlobalHourlyLimit {
		return Challenge{}, ErrGlobalRateLimited
	}

	f.reserve(f.emailRates, rateSubjectKey, challenge.Token, now)
	f.reserve(f.ipRates, rateIPKey, challenge.Token, now)
	f.reserve(f.emailRates, globalKey, challenge.Token, now)
	f.cooldowns[subjectKey] = fakeCooldown{token: challenge.Token, expiresAt: challenge.ResendAfter}
	f.entries[f.entryKey(purpose, challenge.Token)] = &fakeEntry{
		purpose: purpose, token: challenge.Token, subjectKey: subjectKey,
		rateSubjectKey: rateSubjectKey, rateIPKey: rateIPKey,
		codeDigest: codeDigest(f.cfg.Pepper, purpose, challenge.Token, challenge.Code),
		expiresAt:  challenge.ExpiresAt, state: "pending", registration: registration, reset: reset,
	}
	return challenge, nil
}

func (f *Fake) Activate(_ context.Context, purpose Purpose, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanup(f.cfg.Now())
	entry, ok := f.entries[f.entryKey(purpose, token)]
	if !ok {
		return ErrExpired
	}
	if entry.state != "pending" {
		return ErrStateConflict
	}
	if oldToken := f.active[entry.subjectKey]; oldToken != "" && oldToken != token {
		delete(f.entries, f.entryKey(purpose, oldToken))
	}
	entry.state = "active"
	f.active[entry.subjectKey] = token
	return nil
}

func (f *Fake) Cancel(_ context.Context, purpose Purpose, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[f.entryKey(purpose, token)]
	if !ok {
		return nil
	}
	if entry.state != "pending" {
		return ErrStateConflict
	}
	delete(f.entries, f.entryKey(purpose, token))
	if cooldown, ok := f.cooldowns[entry.subjectKey]; ok && cooldown.token == token {
		delete(f.cooldowns, entry.subjectKey)
	}
	f.unreserve(f.emailRates, entry.rateSubjectKey, token)
	f.unreserve(f.ipRates, entry.rateIPKey, token)
	f.unreserve(f.emailRates, "global", token)
	return nil
}

func (f *Fake) VerifyRegistration(_ context.Context, token, code string) (Claim, error) {
	return f.verify(PurposeRegistration, token, code)
}

func (f *Fake) VerifyReset(_ context.Context, token, code string) (Claim, error) {
	return f.verify(PurposeReset, token, code)
}

func (f *Fake) verify(purpose Purpose, token, code string) (Claim, error) {
	leaseID, err := randomToken(f.cfg.Rand)
	if err != nil {
		return Claim{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanup(f.cfg.Now())
	entry, ok := f.entries[f.entryKey(purpose, token)]
	if !ok || entry.state == "pending" || f.active[entry.subjectKey] != token {
		return Claim{}, ErrExpired
	}
	if entry.state == "exhausted" {
		return Claim{}, ErrAttemptsExhausted
	}
	if entry.state == "claimed" {
		return Claim{}, ErrClaimed
	}
	got := codeDigest(f.cfg.Pepper, purpose, token, code)
	if !hmac.Equal([]byte(got), []byte(entry.codeDigest)) {
		entry.attempts++
		if entry.attempts >= f.cfg.MaxAttempts {
			entry.state = "exhausted"
			return Claim{}, ErrAttemptsExhausted
		}
		return Claim{}, ErrInvalidCode
	}
	entry.state = "claimed"
	entry.leaseID = leaseID
	return Claim{
		Purpose: purpose, Token: token, LeaseID: leaseID,
		Registration: entry.registration, Reset: entry.reset,
	}, nil
}

func (f *Fake) Consume(_ context.Context, claim Claim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[f.entryKey(claim.Purpose, claim.Token)]
	if !ok {
		return ErrExpired
	}
	if entry.state != "claimed" || entry.leaseID != claim.LeaseID {
		return ErrStateConflict
	}
	delete(f.entries, f.entryKey(claim.Purpose, claim.Token))
	if f.active[entry.subjectKey] == claim.Token {
		delete(f.active, entry.subjectKey)
	}
	return nil
}

func (f *Fake) Release(_ context.Context, claim Claim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[f.entryKey(claim.Purpose, claim.Token)]
	if !ok {
		return ErrExpired
	}
	if entry.state != "claimed" || entry.leaseID != claim.LeaseID {
		return ErrStateConflict
	}
	entry.state = "active"
	entry.leaseID = ""
	return nil
}

func (f *Fake) cleanup(now time.Time) {
	for key, entry := range f.entries {
		if !now.Before(entry.expiresAt) {
			delete(f.entries, key)
			if f.active[entry.subjectKey] == entry.token {
				delete(f.active, entry.subjectKey)
			}
		}
	}
	for key, cooldown := range f.cooldowns {
		if !now.Before(cooldown.expiresAt) {
			delete(f.cooldowns, key)
		}
	}
	f.cleanupWindows(f.emailRates, now)
	f.cleanupWindows(f.ipRates, now)
}

func (f *Fake) cleanupWindows(windows map[string]*fakeWindow, now time.Time) {
	for key, window := range windows {
		if !now.Before(window.expiresAt) {
			delete(windows, key)
		}
	}
}

func (f *Fake) windowCount(windows map[string]*fakeWindow, key string, now time.Time) int {
	window, ok := windows[key]
	if !ok || !now.Before(window.expiresAt) {
		return 0
	}
	return len(window.tokens)
}

func (f *Fake) reserve(windows map[string]*fakeWindow, key, token string, now time.Time) {
	window, ok := windows[key]
	if !ok || !now.Before(window.expiresAt) {
		window = &fakeWindow{expiresAt: now.Add(f.cfg.RateWindow), tokens: make(map[string]struct{})}
		windows[key] = window
	}
	window.tokens[token] = struct{}{}
}

func (f *Fake) unreserve(windows map[string]*fakeWindow, key, token string) {
	if window := windows[key]; window != nil {
		delete(window.tokens, token)
	}
}

func (f *Fake) entryKey(purpose Purpose, token string) string {
	return string(purpose) + ":" + token
}
