package verification

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

type Purpose string

const (
	PurposeRegistration Purpose = "registration"
	PurposeReset        Purpose = "reset"
)

var (
	ErrUnavailable       = errors.New("verification store unavailable")
	ErrInvalidCode       = errors.New("verification code invalid")
	ErrExpired           = errors.New("verification challenge expired")
	ErrAttemptsExhausted = errors.New("verification attempts exhausted")
	ErrResendTooSoon     = errors.New("verification resend too soon")
	ErrEmailRateLimited  = errors.New("verification email rate limited")
	ErrIPRateLimited     = errors.New("verification ip rate limited")
	ErrGlobalRateLimited = errors.New("verification global rate limited")
	ErrClaimed           = errors.New("verification challenge already claimed")
	ErrStateConflict     = errors.New("verification challenge state conflict")
)

// Config controls short-lived verification state. Pepper must be deployment secret material;
// codes are stored only as HMAC-SHA256 digests.
type Config struct {
	Pepper            []byte
	TTL               time.Duration
	MaxAttempts       int
	ResendAfter       time.Duration
	EmailHourlyLimit  int
	IPHourlyLimit     int
	GlobalHourlyLimit int
	RateWindow        time.Duration
	KeyPrefix         string
	Now               func() time.Time
	Rand              io.Reader
}

func (c Config) normalized() Config {
	if c.TTL <= 0 {
		c.TTL = 10 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.ResendAfter <= 0 {
		c.ResendAfter = time.Minute
	}
	if c.EmailHourlyLimit <= 0 {
		c.EmailHourlyLimit = 5
	}
	if c.IPHourlyLimit <= 0 {
		c.IPHourlyLimit = 20
	}
	if c.GlobalHourlyLimit <= 0 {
		c.GlobalHourlyLimit = 200
	}
	if c.RateWindow <= 0 {
		c.RateWindow = time.Hour
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = "verification"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	return c
}

type RegistrationData struct {
	Email        string
	Nickname     string
	PasswordHash string
}

type ResetData struct {
	Email string
	Valid bool
}

// Challenge includes Code only for delivery by the service. API layers must return Token and the
// timestamps, never Code.
type Challenge struct {
	Purpose     Purpose
	Token       string
	Code        string
	ExpiresAt   time.Time
	ResendAfter time.Time
}

// Claim is an atomic verification lease. Durable work happens while the lease is held; callers
// consume it after success and release it after a transient durable-store failure.
type Claim struct {
	Purpose      Purpose
	Token        string
	LeaseID      string
	Registration RegistrationData
	Reset        ResetData
}

// Store separates pending delivery, activation, verification claiming, and final consumption.
// This lets SMTP failures CAS-cancel only their pending generation and its cooldown/rate slots.
type Store interface {
	StartRegistration(context.Context, RegistrationData, string) (Challenge, error)
	StartReset(context.Context, ResetData, string) (Challenge, error)
	Activate(context.Context, Purpose, string) error
	Cancel(context.Context, Purpose, string) error
	VerifyRegistration(context.Context, string, string) (Claim, error)
	VerifyReset(context.Context, string, string) (Claim, error)
	Consume(context.Context, Claim) error
	Release(context.Context, Claim) error
}

func newChallenge(c Config, purpose Purpose) (Challenge, error) {
	token, err := randomToken(c.Rand)
	if err != nil {
		return Challenge{}, fmt.Errorf("generate verification token: %w", err)
	}
	code, err := randomCode(c.Rand)
	if err != nil {
		return Challenge{}, fmt.Errorf("generate verification code: %w", err)
	}
	now := c.Now()
	return Challenge{
		Purpose: purpose, Token: token, Code: code,
		ExpiresAt: now.Add(c.TTL), ResendAfter: now.Add(c.ResendAfter),
	}, nil
}

func randomToken(source io.Reader) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(source, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomCode(source io.Reader) (string, error) {
	n, err := rand.Int(source, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func codeDigest(pepper []byte, purpose Purpose, token, code string) string {
	mac := hmac.New(sha256.New, pepper)
	_, _ = io.WriteString(mac, string(purpose))
	_, _ = io.WriteString(mac, "\x00")
	_, _ = io.WriteString(mac, token)
	_, _ = io.WriteString(mac, "\x00")
	_, _ = io.WriteString(mac, code)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func subjectDigest(pepper []byte, purpose Purpose, subject string) string {
	mac := hmac.New(sha256.New, pepper)
	_, _ = io.WriteString(mac, "subject\x00")
	_, _ = io.WriteString(mac, string(purpose))
	_, _ = io.WriteString(mac, "\x00")
	_, _ = io.WriteString(mac, strings.ToLower(strings.TrimSpace(subject)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func rateSubjectDigest(pepper []byte, subject string) string {
	mac := hmac.New(sha256.New, pepper)
	_, _ = io.WriteString(mac, "mail-rate\x00")
	_, _ = io.WriteString(mac, strings.ToLower(strings.TrimSpace(subject)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
