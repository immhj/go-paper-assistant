package guard

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestFakeClassifiesConcurrentRateCostAndUnavailable(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	fake := NewFake(Config{Now: func() time.Time { return now }, CostHourlyUSD: 2, CostDailyUSD: 10})
	ctx := context.Background()

	lease, err := fake.BeginTurn(ctx, "account:1", "192.0.2.1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = fake.BeginTurn(ctx, "account:1", "192.0.2.1"); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("want concurrent, got %v", err)
	}
	if err = fake.Release(ctx, Lease{ActorKey: lease.ActorKey, Token: "wrong"}); err != nil {
		t.Fatalf("wrong-token release: %v", err)
	}
	if _, err = fake.BeginTurn(ctx, "account:1", "192.0.2.1"); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("wrong token released lease: %v", err)
	}
	if err = fake.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	for index := 1; index < actorPerMinute; index++ {
		item, beginErr := fake.BeginTurn(ctx, "account:1", "192.0.2.1")
		if beginErr != nil {
			t.Fatalf("rate request %d: %v", index+1, beginErr)
		}
		_ = fake.Release(ctx, item)
	}
	if _, err = fake.BeginTurn(ctx, "account:1", "192.0.2.1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want rate limited, got %v", err)
	}

	now = now.Add(time.Minute)
	if err = fake.RecordCost(ctx, 2); err != nil {
		t.Fatalf("record cost: %v", err)
	}
	if _, err = fake.BeginTurn(ctx, "account:2", "192.0.2.2"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("want circuit open, got %v", err)
	}
	fake.SetUnavailable(true)
	if _, err = fake.BeginTurn(ctx, "account:3", "192.0.2.3"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestFakeToolRateClasses(t *testing.T) {
	fake := NewFake(Config{})
	ctx := context.Background()
	for index := 0; index < searchQuickPerMinute; index++ {
		if err := fake.AllowTool(ctx, "guest:1", ToolSearchOrQuick); err != nil {
			t.Fatalf("search/quick %d: %v", index, err)
		}
	}
	if err := fake.AllowTool(ctx, "guest:1", ToolSearchOrQuick); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want search/quick limit, got %v", err)
	}
	for index := 0; index < accountDeepPerMinute; index++ {
		if err := fake.AllowTool(ctx, "account:1", ToolAccountDeep); err != nil {
			t.Fatalf("deep %d: %v", index, err)
		}
	}
	if err := fake.AllowTool(ctx, "account:1", ToolAccountDeep); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want deep limit, got %v", err)
	}
}

func TestFakeEnforcesSharedIPLimit(t *testing.T) {
	fake := NewFake(Config{})
	ctx := context.Background()
	for index := 0; index < ipPerMinute; index++ {
		actor := fmt.Sprintf("account:%d", index+1)
		lease, err := fake.BeginTurn(ctx, actor, "198.51.100.7")
		if err != nil {
			t.Fatalf("IP request %d: %v", index+1, err)
		}
		if err = fake.Release(ctx, lease); err != nil {
			t.Fatalf("release %d: %v", index+1, err)
		}
	}
	if _, err := fake.BeginTurn(ctx, "account:99", "198.51.100.7"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want shared IP rate limit, got %v", err)
	}
}
