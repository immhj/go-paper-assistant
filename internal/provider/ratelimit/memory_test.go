package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestMemoryFixedWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := NewMemory(func() time.Time { return now })
	ctx := context.Background()
	key := Key("test", "198.51.100.1")

	for want := int64(1); want <= 3; want++ {
		count, ttl, err := m.Hit(ctx, key, time.Minute)
		if err != nil || count != want || ttl != time.Minute {
			t.Fatalf("hit %d: count=%d ttl=%v err=%v", want, count, ttl, err)
		}
	}
	now = now.Add(20 * time.Second)
	if count, ttl, _ := m.Count(ctx, key); count != 3 || ttl != 40*time.Second {
		t.Fatalf("count mid-window: count=%d ttl=%v", count, ttl)
	}
	now = now.Add(40 * time.Second)
	if count, _, _ := m.Count(ctx, key); count != 0 {
		t.Fatalf("count after window: %d", count)
	}
	m.Hit(ctx, key, time.Minute)
	if err := m.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}
	if count, _, _ := m.Count(ctx, key); count != 0 {
		t.Fatalf("count after reset: %d", count)
	}
}

func TestKeyHashesValue(t *testing.T) {
	key := Key("login:email", "alice@example.com")
	if want := "paperassistant:rl:login:email:"; key[:len(want)] != want || len(key) != len(want)+32 {
		t.Fatalf("unexpected key %q", key)
	}
	if Key("login:email", "alice@example.com") != key || Key("login:email", "bob@example.com") == key {
		t.Fatal("key is not a deterministic per-value digest")
	}
}
