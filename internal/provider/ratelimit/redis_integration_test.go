//go:build integration

package ratelimit

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// REDIS_TEST_ADDR must point at a disposable Redis instance. This verifies the production Lua
// counter semantics, not merely that redis-cli can answer PING.
func TestRedisHitWindowAtomicity_Integration(t *testing.T) {
	address := os.Getenv("REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("REDIS_TEST_ADDR is not set")
	}
	client := redis.NewClient(&redis.Options{Addr: address})
	defer client.Close()
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	key := "paperassistant:integration:" + randomKey(t)
	store := NewRedis(client)
	t.Cleanup(func() { _ = store.Reset(context.Background(), key) })
	first, firstTTL, err := store.Hit(ctx, key, time.Minute)
	if err != nil || first != 1 || firstTTL <= 0 || firstTTL > time.Minute {
		t.Fatalf("first hit count=%d ttl=%s err=%v", first, firstTTL, err)
	}
	second, secondTTL, err := store.Hit(ctx, key, time.Minute)
	if err != nil || second != 2 || secondTTL <= 0 || secondTTL > firstTTL {
		t.Fatalf("second hit count=%d ttl=%s firstTTL=%s err=%v", second, secondTTL, firstTTL, err)
	}
	count, ttl, err := store.Count(ctx, key)
	if err != nil || count != 2 || ttl <= 0 {
		t.Fatalf("count=%d ttl=%s err=%v", count, ttl, err)
	}
	if err = store.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}
	count, ttl, err = store.Count(ctx, key)
	if err != nil || count != 0 || ttl != 0 {
		t.Fatalf("after reset count=%d ttl=%s err=%v", count, ttl, err)
	}
}

func randomKey(t *testing.T) string {
	t.Helper()
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", value)
}
