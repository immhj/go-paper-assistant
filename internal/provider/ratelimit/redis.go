package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Redis struct{ client redis.UniversalClient }

func NewRedis(client redis.UniversalClient) *Redis { return &Redis{client: client} }

// hitScript increments and sets the window TTL atomically on the first hit, so a crash between
// INCR and PEXPIRE can never leave an immortal counter.
var hitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]); ttl = tonumber(ARGV[1]) end
return {count, ttl}
`)

func (r *Redis) Hit(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	values, err := hitScript.Run(ctx, r.client, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil || len(values) != 2 {
		return 0, 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return values[0], time.Duration(values[1]) * time.Millisecond, nil
}

func (r *Redis) Count(ctx context.Context, key string) (int64, time.Duration, error) {
	pipe := r.client.Pipeline()
	get := pipe.Get(ctx, key)
	ttl := pipe.PTTL(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	count, err := get.Int64()
	if err == redis.Nil {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return count, max(ttl.Val(), 0), nil
}

func (r *Redis) Reset(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}
