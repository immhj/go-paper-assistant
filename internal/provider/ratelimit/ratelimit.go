// Package ratelimit provides fixed-window counters used for login-failure throttling and the
// per-IP API budget. Keys are hashed before storage so raw emails and IPs never land in Redis.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

var ErrUnavailable = errors.New("rate limit store unavailable")

// Store is a set of fixed-window counters. A window starts at the first Hit on a key and the
// counter disappears when it ends.
type Store interface {
	// Hit increments key and returns the new count and the time left in its window.
	Hit(ctx context.Context, key string, window time.Duration) (count int64, ttl time.Duration, err error)
	// Count returns the current count and time left without incrementing; a missing key is 0.
	Count(ctx context.Context, key string) (count int64, ttl time.Duration, err error)
	// Reset deletes key.
	Reset(ctx context.Context, key string) error
}

// Key builds a namespaced, hashed counter key, e.g. Key("login:email", "a@b.c").
func Key(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "paperassistant:rl:" + kind + ":" + hex.EncodeToString(sum[:16])
}
