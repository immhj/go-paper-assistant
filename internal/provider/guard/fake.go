package guard

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type fakeWindow struct {
	started time.Time
	count   int
}

type Fake struct {
	mu          sync.Mutex
	cfg         Config
	rates       map[string]fakeWindow
	leases      map[string]Lease
	hourlyCost  map[string]float64
	dailyCost   map[string]float64
	unavailable bool
}

func NewFake(cfg Config) *Fake {
	return &Fake{cfg: cfg.normalized(), rates: make(map[string]fakeWindow), leases: make(map[string]Lease), hourlyCost: make(map[string]float64), dailyCost: make(map[string]float64)}
}

func (f *Fake) SetUnavailable(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unavailable = value
}

func (f *Fake) BeginTurn(_ context.Context, actorKey, ip string) (Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable || actorKey == "" || ip == "" {
		return Lease{}, ErrUnavailable
	}
	now := f.cfg.Now().UTC()
	if f.hourlyCost[now.Format("2006010215")] >= f.cfg.CostHourlyUSD || f.dailyCost[now.Format("20060102")] >= f.cfg.CostDailyUSD {
		return Lease{}, ErrCircuitOpen
	}
	ipKey, actorRateKey := "ip:"+ip, "actor:"+actorKey
	if f.atLimit(ipKey, ipPerMinute, now) || f.atLimit(actorRateKey, actorPerMinute, now) {
		return Lease{}, ErrRateLimited
	}
	if lease, ok := f.leases[actorKey]; ok {
		if now.Sub(f.rates["lease:"+lease.Token].started) < f.cfg.LeaseTTL {
			return Lease{}, ErrConcurrent
		}
		delete(f.leases, actorKey)
		delete(f.rates, "lease:"+lease.Token)
	}
	token, err := randomToken()
	if err != nil {
		return Lease{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	f.increment(ipKey, now)
	f.increment(actorRateKey, now)
	lease := Lease{ActorKey: actorKey, Token: token}
	f.leases[actorKey] = lease
	f.rates["lease:"+token] = fakeWindow{started: now}
	return lease, nil
}

func (f *Fake) AllowTool(_ context.Context, actorKey string, class ToolClass) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return ErrUnavailable
	}
	limit := 0
	switch class {
	case ToolSearchOrQuick:
		limit = searchQuickPerMinute
	case ToolAccountDeep:
		limit = accountDeepPerMinute
	default:
		return nil
	}
	key := "tool:" + string(class) + ":" + actorKey
	now := f.cfg.Now().UTC()
	if f.atLimit(key, limit, now) {
		return ErrRateLimited
	}
	f.increment(key, now)
	return nil
}

func (f *Fake) Release(_ context.Context, lease Lease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return ErrUnavailable
	}
	if current, ok := f.leases[lease.ActorKey]; ok && current.Token == lease.Token {
		delete(f.leases, lease.ActorKey)
		delete(f.rates, "lease:"+lease.Token)
	}
	return nil
}

func (f *Fake) RecordCost(_ context.Context, costUSD float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return ErrUnavailable
	}
	if costUSD <= 0 {
		return nil
	}
	now := f.cfg.Now().UTC()
	f.hourlyCost[now.Format("2006010215")] += costUSD
	f.dailyCost[now.Format("20060102")] += costUSD
	return nil
}

func (f *Fake) atLimit(key string, limit int, now time.Time) bool {
	window := f.rates[key]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		return false
	}
	return window.count >= limit
}

func (f *Fake) increment(key string, now time.Time) {
	window := f.rates[key]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		window = fakeWindow{started: now}
	}
	window.count++
	f.rates[key] = window
}
