package verification

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore implements Store with Lua-guarded state transitions and fixed-window rate slots.
type RedisStore struct {
	client redis.UniversalClient
	cfg    Config
}

func NewRedis(client redis.UniversalClient, cfg Config) (*RedisStore, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}
	if len(cfg.Pepper) < 16 {
		return nil, errors.New("verification pepper must be at least 16 bytes")
	}
	cfg = cfg.normalized()
	return &RedisStore{client: client, cfg: cfg}, nil
}

func (r *RedisStore) StartRegistration(ctx context.Context, data RegistrationData, clientIP string) (Challenge, error) {
	return r.start(ctx, PurposeRegistration, data.Email, clientIP, data, ResetData{})
}

func (r *RedisStore) StartReset(ctx context.Context, data ResetData, clientIP string) (Challenge, error) {
	return r.start(ctx, PurposeReset, data.Email, clientIP, RegistrationData{}, data)
}

func (r *RedisStore) start(ctx context.Context, purpose Purpose, subject, clientIP string, registration RegistrationData, reset ResetData) (Challenge, error) {
	challenge, err := newChallenge(r.cfg, purpose)
	if err != nil {
		return Challenge{}, err
	}
	subjectID := subjectDigest(r.cfg.Pepper, purpose, subject)
	rateSubjectID := rateSubjectDigest(r.cfg.Pepper, subject)
	rateIPID := rateSubjectDigest(r.cfg.Pepper, clientIP)
	keys := []string{
		r.challengeKey(purpose, challenge.Token), r.cooldownKey(purpose, subjectID),
		r.emailRateKey(rateSubjectID), r.ipRateKey(rateIPID), r.globalRateKey(),
	}
	resetValid := 0
	if reset.Valid {
		resetValid = 1
	}
	result, err := startScript.Run(ctx, r.client, keys,
		challenge.Token, registration.Email, registration.Nickname, registration.PasswordHash,
		reset.Email, resetValid, codeDigest(r.cfg.Pepper, purpose, challenge.Token, challenge.Code),
		durationMillis(r.cfg.TTL), durationMillis(r.cfg.ResendAfter), durationMillis(r.cfg.RateWindow),
		r.cfg.EmailHourlyLimit, r.cfg.IPHourlyLimit, r.cfg.GlobalHourlyLimit, subjectID, rateSubjectID, rateIPID,
	).Int64()
	if err != nil {
		return Challenge{}, unavailable(err)
	}
	switch result {
	case 0:
		return challenge, nil
	case 1:
		return Challenge{}, ErrResendTooSoon
	case 2:
		return Challenge{}, ErrEmailRateLimited
	case 3:
		return Challenge{}, ErrIPRateLimited
	case 4:
		return Challenge{}, ErrGlobalRateLimited
	default:
		return Challenge{}, ErrStateConflict
	}
}

func (r *RedisStore) Activate(ctx context.Context, purpose Purpose, token string) error {
	key := r.challengeKey(purpose, token)
	values, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return unavailable(err)
	}
	if len(values) == 0 {
		return ErrExpired
	}
	activeKey := r.activeKey(purpose, values["subject_id"])
	result, err := activateScript.Run(ctx, r.client, []string{key, activeKey}, token).Int64()
	if err != nil {
		return unavailable(err)
	}
	return stateResult(result)
}

func (r *RedisStore) Cancel(ctx context.Context, purpose Purpose, token string) error {
	key := r.challengeKey(purpose, token)
	values, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return unavailable(err)
	}
	if len(values) == 0 {
		return nil
	}
	keys := []string{
		key, r.cooldownKey(purpose, values["subject_id"]),
		r.emailRateKey(values["rate_subject_id"]), r.ipRateKey(values["rate_ip_id"]), r.globalRateKey(),
	}
	result, err := cancelScript.Run(ctx, r.client, keys, token).Int64()
	if err != nil {
		return unavailable(err)
	}
	if result == 0 || result == 1 {
		return nil
	}
	return ErrStateConflict
}

func (r *RedisStore) VerifyRegistration(ctx context.Context, token, code string) (Claim, error) {
	return r.verify(ctx, PurposeRegistration, token, code)
}

func (r *RedisStore) VerifyReset(ctx context.Context, token, code string) (Claim, error) {
	return r.verify(ctx, PurposeReset, token, code)
}

func (r *RedisStore) verify(ctx context.Context, purpose Purpose, token, code string) (Claim, error) {
	leaseID, err := randomToken(r.cfg.Rand)
	if err != nil {
		return Claim{}, err
	}
	key := r.challengeKey(purpose, token)
	values, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return Claim{}, unavailable(err)
	}
	if len(values) == 0 {
		return Claim{}, ErrExpired
	}
	result, err := verifyScript.Run(ctx, r.client,
		[]string{key, r.activeKey(purpose, values["subject_id"])},
		token, codeDigest(r.cfg.Pepper, purpose, token, code), leaseID, r.cfg.MaxAttempts,
	).Int64()
	if err != nil {
		return Claim{}, unavailable(err)
	}
	switch result {
	case 0:
		return Claim{
			Purpose: purpose, Token: token, LeaseID: leaseID,
			Registration: RegistrationData{Email: values["email"], Nickname: values["nickname"], PasswordHash: values["password_hash"]},
			Reset:        ResetData{Email: values["reset_email"], Valid: values["reset_valid"] == "1"},
		}, nil
	case 1:
		return Claim{}, ErrExpired
	case 2:
		return Claim{}, ErrInvalidCode
	case 3:
		return Claim{}, ErrAttemptsExhausted
	case 4:
		return Claim{}, ErrClaimed
	default:
		return Claim{}, ErrStateConflict
	}
}

func (r *RedisStore) Consume(ctx context.Context, claim Claim) error {
	key := r.challengeKey(claim.Purpose, claim.Token)
	values, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return unavailable(err)
	}
	if len(values) == 0 {
		return ErrExpired
	}
	result, err := consumeScript.Run(ctx, r.client,
		[]string{key, r.activeKey(claim.Purpose, values["subject_id"])}, claim.Token, claim.LeaseID,
	).Int64()
	if err != nil {
		return unavailable(err)
	}
	return stateResult(result)
}

func (r *RedisStore) Release(ctx context.Context, claim Claim) error {
	result, err := releaseScript.Run(ctx, r.client,
		[]string{r.challengeKey(claim.Purpose, claim.Token)}, claim.LeaseID,
	).Int64()
	if err != nil {
		return unavailable(err)
	}
	return stateResult(result)
}

func (r *RedisStore) challengeKey(purpose Purpose, token string) string {
	return r.cfg.KeyPrefix + ":challenge:" + string(purpose) + ":" + token
}
func (r *RedisStore) activeKey(purpose Purpose, subjectID string) string {
	return r.cfg.KeyPrefix + ":active:" + string(purpose) + ":" + subjectID
}
func (r *RedisStore) cooldownKey(purpose Purpose, subjectID string) string {
	return r.cfg.KeyPrefix + ":cooldown:" + string(purpose) + ":" + subjectID
}
func (r *RedisStore) emailRateKey(subjectID string) string {
	return r.cfg.KeyPrefix + ":ratelimit:email:" + subjectID
}
func (r *RedisStore) ipRateKey(ipID string) string {
	return r.cfg.KeyPrefix + ":ratelimit:ip:" + ipID
}
func (r *RedisStore) globalRateKey() string {
	return r.cfg.KeyPrefix + ":ratelimit:global"
}

func durationMillis(value time.Duration) int64 {
	millis := value.Milliseconds()
	if millis < 1 {
		return 1
	}
	return millis
}

func unavailable(err error) error {
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func stateResult(result int64) error {
	switch result {
	case 0:
		return nil
	case 1:
		return ErrExpired
	default:
		return ErrStateConflict
	}
}

var startScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then return 1 end
local email_count = tonumber(redis.call('GET', KEYS[3]) or '0')
if email_count >= tonumber(ARGV[11]) then return 2 end
local ip_count = tonumber(redis.call('GET', KEYS[4]) or '0')
if ip_count >= tonumber(ARGV[12]) then return 3 end
local global_count = tonumber(redis.call('GET', KEYS[5]) or '0')
if global_count >= tonumber(ARGV[13]) then return 4 end
redis.call('HSET', KEYS[1],
  'state', 'pending', 'token', ARGV[1], 'email', ARGV[2], 'nickname', ARGV[3],
  'password_hash', ARGV[4], 'reset_email', ARGV[5], 'reset_valid', ARGV[6],
  'code_digest', ARGV[7], 'subject_id', ARGV[14], 'rate_subject_id', ARGV[15],
  'rate_ip_id', ARGV[16], 'attempts', '0')
redis.call('PEXPIRE', KEYS[1], ARGV[8])
redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[9])
local new_email_count = redis.call('INCR', KEYS[3])
if new_email_count == 1 then redis.call('PEXPIRE', KEYS[3], ARGV[10]) end
local new_ip_count = redis.call('INCR', KEYS[4])
if new_ip_count == 1 then redis.call('PEXPIRE', KEYS[4], ARGV[10]) end
local new_global_count = redis.call('INCR', KEYS[5])
if new_global_count == 1 then redis.call('PEXPIRE', KEYS[5], ARGV[10]) end
return 0
`)

var activateScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
if redis.call('HGET', KEYS[1], 'state') ~= 'pending' then return 2 end
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then return 1 end
redis.call('HSET', KEYS[1], 'state', 'active')
redis.call('SET', KEYS[2], ARGV[1], 'PX', ttl)
return 0
`)

var cancelScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
if redis.call('HGET', KEYS[1], 'state') ~= 'pending' then return 2 end
redis.call('DEL', KEYS[1])
if redis.call('GET', KEYS[2]) == ARGV[1] then redis.call('DEL', KEYS[2]) end
local email_count = tonumber(redis.call('GET', KEYS[3]) or '0')
if email_count <= 1 then redis.call('DEL', KEYS[3]) else redis.call('DECR', KEYS[3]) end
local ip_count = tonumber(redis.call('GET', KEYS[4]) or '0')
if ip_count <= 1 then redis.call('DEL', KEYS[4]) else redis.call('DECR', KEYS[4]) end
local global_count = tonumber(redis.call('GET', KEYS[5]) or '0')
if global_count <= 1 then redis.call('DEL', KEYS[5]) else redis.call('DECR', KEYS[5]) end
return 0
`)

var verifyScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
local state = redis.call('HGET', KEYS[1], 'state')
if state == 'exhausted' then return 3 end
if state == 'claimed' then return 4 end
if state ~= 'active' or redis.call('GET', KEYS[2]) ~= ARGV[1] then return 1 end
if redis.call('HGET', KEYS[1], 'code_digest') ~= ARGV[2] then
  local attempts = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
  if attempts >= tonumber(ARGV[4]) then
    redis.call('HSET', KEYS[1], 'state', 'exhausted')
    return 3
  end
  return 2
end
redis.call('HSET', KEYS[1], 'state', 'claimed', 'lease_id', ARGV[3])
return 0
`)

var consumeScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
if redis.call('HGET', KEYS[1], 'state') ~= 'claimed' or redis.call('HGET', KEYS[1], 'lease_id') ~= ARGV[2] then return 2 end
redis.call('DEL', KEYS[1])
if redis.call('GET', KEYS[2]) == ARGV[1] then redis.call('DEL', KEYS[2]) end
return 0
`)

var releaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
if redis.call('HGET', KEYS[1], 'state') ~= 'claimed' or redis.call('HGET', KEYS[1], 'lease_id') ~= ARGV[1] then return 2 end
redis.call('HSET', KEYS[1], 'state', 'active')
redis.call('HDEL', KEYS[1], 'lease_id')
return 0
`)
