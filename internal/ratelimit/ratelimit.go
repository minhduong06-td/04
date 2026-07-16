package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"hustack/internal/config"
)

type RateLimiter struct {
	client *redis.Client
	cfg    *config.Config
	ctx    context.Context
	now    func() time.Time
}

func New(client *redis.Client, cfg *config.Config) *RateLimiter {
	return &RateLimiter{
		client: client,
		cfg:    cfg,
		ctx:    context.Background(),
		now:    time.Now,
	}
}

type Clock interface {
	Now() time.Time
}

func (rl *RateLimiter) SetClock(c Clock) {
	rl.now = c.Now
}

func (rl *RateLimiter) AllowParticipantSubmit(participantID string) (bool, int, error) {
	retryAfter, err := rl.tokenBucket(
		"rl:submit:min:"+participantID,
		rl.cfg.SubmitRatePerMinute,
		time.Minute,
		1,
	)
	if err != nil {
		return false, 0, err
	}
	if retryAfter > 0 {
		return false, retryAfter, nil
	}

	retryAfter, err = rl.tokenBucket(
		"rl:submit:hour:"+participantID,
		rl.cfg.SubmitRatePerHour,
		time.Hour,
		1,
	)
	if err != nil {
		return false, 0, err
	}
	if retryAfter > 0 {
		return false, retryAfter, nil
	}

	return true, 0, nil
}

func (rl *RateLimiter) AllowIPSubmit(ip string) (bool, int, error) {
	retryAfter, err := rl.tokenBucket(
		"rl:ip:submit:"+ip,
		rl.cfg.IPSubmitRatePerMinute,
		time.Minute,
		1,
	)
	if err != nil {
		return false, 0, err
	}
	if retryAfter > 0 {
		return false, retryAfter, nil
	}
	return true, 0, nil
}

func (rl *RateLimiter) AllowPoll(participantID string) (bool, int, error) {
	retryAfter, err := rl.tokenBucket(
		"rl:poll:"+participantID,
		rl.cfg.PollRatePerMinute,
		time.Minute,
		1,
	)
	if err != nil {
		return false, 0, err
	}
	if retryAfter > 0 {
		return false, retryAfter, nil
	}
	return true, 0, nil
}

func (rl *RateLimiter) tokenBucket(key string, rate int, period time.Duration, cost int) (int, error) {
	if rate <= 0 {
		return 0, nil
	}

	now := rl.now().UnixMilli()
	intervalMs := period.Milliseconds() / int64(rate)
	if intervalMs < 1 {
		intervalMs = 1
	}
	capacity := int64(rate)

	script := `
local key = KEYS[1]
local now = tonumber(ARGV[1])
local interval = tonumber(ARGV[2])
local capacity = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local tokens = redis.call("GET", key)
if not tokens then
    tokens = capacity
    redis.call("SET", key .. ":ts", now)
    redis.call("SET", key, tokens)
end

local last_refill = redis.call("GET", key .. ":ts")
if not last_refill then
    last_refill = now
end

last_refill = tonumber(last_refill)
local elapsed = now - last_refill
local refill = math.floor(elapsed / interval)
if refill > 0 then
    tokens = tonumber(tokens)
    tokens = math.min(capacity, tokens + refill)
    local used_intervals = refill * interval
    redis.call("SET", key .. ":ts", last_refill + used_intervals)
    redis.call("SET", key, tokens)
end

tokens = tonumber(tokens)
if tokens >= cost then
    tokens = tokens - cost
    redis.call("SET", key, tokens)
    redis.call("PEXPIRE", key, math.max(1000, interval * capacity * 2))
    redis.call("PEXPIRE", key .. ":ts", math.max(1000, interval * capacity * 2))
    return 0
else
    local wait_ms = interval * (cost - tokens)
    local retry_sec = math.ceil(wait_ms / 1000)
    if retry_sec < 1 then retry_sec = 1 end
    return retry_sec
end
`

	retryAfter, err := rl.client.Eval(rl.ctx, script, []string{key}, now, intervalMs, capacity, cost).Int()
	if err != nil {
		return 0, fmt.Errorf("token bucket: %w", err)
	}
	return retryAfter, nil
}

func (rl *RateLimiter) FlushDB(ctx context.Context) error {
	return rl.client.FlushDB(ctx).Err()
}

func (rl *RateLimiter) Ping(ctx context.Context) error {
	return rl.client.Ping(ctx).Err()
}

func (rl *RateLimiter) Close() error {
	return rl.client.Close()
}
