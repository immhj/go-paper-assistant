package guard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"paperassistant/internal/metrics"
)

const (
	ipPerMinute          = 30
	actorPerMinute       = 10
	searchQuickPerMinute = 3
	accountDeepPerMinute = 2
)

type Redis struct {
	client redis.UniversalClient
	cfg    Config
}

func NewRedis(client redis.UniversalClient, cfg Config) (*Redis, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}
	cfg = cfg.normalized()
	return &Redis{client: client, cfg: cfg}, nil
}

func (r *Redis) BeginTurn(ctx context.Context, actorKey, ip string) (Lease, error) {
	if actorKey == "" || ip == "" {
		return Lease{}, fmt.Errorf("%w: actor and IP are required", ErrUnavailable)
	}
	token, err := randomToken()
	if err != nil {
		return Lease{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	now := r.cfg.Now().UTC()
	hourKey, dayKey, hourTTL, dayTTL := r.costKeys(now)
	keys := []string{r.rateKey("ip", ip), r.rateKey("actor", actorKey), r.leaseKey(actorKey), hourKey, dayKey}
	result, err := beginTurnScript.Run(ctx, r.client, keys, token, ipPerMinute, actorPerMinute,
		time.Minute.Milliseconds(), r.cfg.LeaseTTL.Milliseconds(), r.cfg.CostHourlyUSD, r.cfg.CostDailyUSD,
		hourTTL.Milliseconds(), dayTTL.Milliseconds()).Int64()
	if err != nil {
		return Lease{}, unavailable(err)
	}
	switch result {
	case 0:
		return Lease{ActorKey: actorKey, Token: token}, nil
	case 1, 2:
		metrics.RateLimited("web_turn")
		return Lease{}, ErrRateLimited
	case 3:
		metrics.RateLimited("web_concurrent")
		return Lease{}, ErrConcurrent
	case 4:
		metrics.RateLimited("cost_breaker")
		return Lease{}, ErrCircuitOpen
	default:
		return Lease{}, ErrUnavailable
	}
}

func (r *Redis) AllowTool(ctx context.Context, actorKey string, class ToolClass) error {
	limit := 0
	switch class {
	case ToolSearchOrQuick:
		limit = searchQuickPerMinute
	case ToolAccountDeep:
		limit = accountDeepPerMinute
	default:
		return nil
	}
	result, err := rateScript.Run(ctx, r.client, []string{r.rateKey(string(class), actorKey)}, limit, time.Minute.Milliseconds()).Int64()
	if err != nil {
		return unavailable(err)
	}
	if result != 0 {
		metrics.RateLimited("web_tool")
		return ErrRateLimited
	}
	return nil
}

func (r *Redis) Release(ctx context.Context, lease Lease) error {
	if lease.ActorKey == "" || lease.Token == "" {
		return nil
	}
	if _, err := releaseScript.Run(ctx, r.client, []string{r.leaseKey(lease.ActorKey)}, lease.Token).Result(); err != nil {
		return unavailable(err)
	}
	return nil
}

func (r *Redis) RecordCost(ctx context.Context, costUSD float64) error {
	if costUSD <= 0 {
		return nil
	}
	now := r.cfg.Now().UTC()
	hourKey, dayKey, hourTTL, dayTTL := r.costKeys(now)
	if _, err := recordCostScript.Run(ctx, r.client, []string{hourKey, dayKey}, costUSD, durationMillis(hourTTL), durationMillis(dayTTL)).Result(); err != nil {
		return unavailable(err)
	}
	return nil
}

func (r *Redis) rateKey(kind, value string) string {
	return r.cfg.KeyPrefix + ":rate:" + kind + ":" + digest(value)
}
func (r *Redis) leaseKey(actorKey string) string {
	return r.cfg.KeyPrefix + ":lease:" + digest(actorKey)
}
func (r *Redis) costKeys(now time.Time) (string, string, time.Duration, time.Duration) {
	hourEnd := now.Truncate(time.Hour).Add(time.Hour)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)
	return r.cfg.KeyPrefix + ":cost:hour:" + now.Format("2006010215"),
		r.cfg.KeyPrefix + ":cost:day:" + now.Format("20060102"), hourEnd.Sub(now), dayEnd.Sub(now)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}
func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
func unavailable(err error) error { return fmt.Errorf("%w: %v", ErrUnavailable, err) }
func durationMillis(value time.Duration) int64 {
	if millis := value.Milliseconds(); millis > 0 {
		return millis
	}
	return 1
}

var beginTurnScript = redis.NewScript(`
local hourly = tonumber(redis.call('GET', KEYS[4]) or '0')
local daily = tonumber(redis.call('GET', KEYS[5]) or '0')
if hourly == nil or daily == nil then return 5 end
if hourly >= tonumber(ARGV[6]) or daily >= tonumber(ARGV[7]) then return 4 end
if tonumber(redis.call('GET', KEYS[1]) or '0') >= tonumber(ARGV[2]) then return 1 end
if tonumber(redis.call('GET', KEYS[2]) or '0') >= tonumber(ARGV[3]) then return 2 end
if redis.call('EXISTS', KEYS[3]) == 1 then return 3 end
local ip_count = redis.call('INCR', KEYS[1])
if ip_count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[4]) end
local actor_count = redis.call('INCR', KEYS[2])
if actor_count == 1 then redis.call('PEXPIRE', KEYS[2], ARGV[4]) end
redis.call('SET', KEYS[3], ARGV[1], 'PX', ARGV[5], 'NX')
return 0
`)

var rateScript = redis.NewScript(`
local count = tonumber(redis.call('GET', KEYS[1]) or '0')
if count >= tonumber(ARGV[1]) then return 1 end
count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 0
`)

var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

var recordCostScript = redis.NewScript(`
local hour = redis.call('INCRBYFLOAT', KEYS[1], ARGV[1])
if redis.call('PTTL', KEYS[1]) < 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
local day = redis.call('INCRBYFLOAT', KEYS[2], ARGV[1])
if redis.call('PTTL', KEYS[2]) < 0 then redis.call('PEXPIRE', KEYS[2], ARGV[3]) end
return {hour, day}
`)
