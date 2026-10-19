package guard

import (
	"context"
	"errors"
	"time"
)

var (
	ErrRateLimited = errors.New("web request rate limited")
	ErrConcurrent  = errors.New("web actor already has an active turn")
	ErrCircuitOpen = errors.New("web model cost circuit is open")
	ErrUnavailable = errors.New("web guard unavailable")
)

type ToolClass string

const (
	ToolSearchOrQuick ToolClass = "search_or_quick"
	ToolAccountDeep   ToolClass = "account_deep"
)

type Lease struct {
	ActorKey string
	Token    string
}

type WebGuard interface {
	BeginTurn(ctx context.Context, actorKey, ip string) (Lease, error)
	AllowTool(ctx context.Context, actorKey string, class ToolClass) error
	Release(ctx context.Context, lease Lease) error
	RecordCost(ctx context.Context, costUSD float64) error
}

type Config struct {
	KeyPrefix     string
	LeaseTTL      time.Duration
	CostHourlyUSD float64
	CostDailyUSD  float64
	Now           func() time.Time
}

func (c Config) normalized() Config {
	if c.KeyPrefix == "" {
		c.KeyPrefix = "paperassistant:web"
	}
	if c.LeaseTTL <= 0 {
		// A Web turn can include router, executor, full-text extraction and a thinking model call.
		// Keep the crash-recovery TTL above that bounded pipeline; normal completion releases early.
		c.LeaseTTL = 30 * time.Minute
	}
	if c.CostHourlyUSD <= 0 {
		c.CostHourlyUSD = 2
	}
	if c.CostDailyUSD <= 0 {
		c.CostDailyUSD = 10
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
