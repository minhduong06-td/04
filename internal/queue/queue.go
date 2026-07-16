package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrQueueFull    = errors.New("queue is at maximum capacity")
	ErrNoJob        = errors.New("no job available")
	ErrQueuedQuota  = errors.New("queued submission quota exceeded")
	ErrRunningQuota = errors.New("running submission quota exceeded")
	ErrInvalidItem  = errors.New("invalid queue item")
)

type JobItem struct {
	SubmissionID  string
	ParticipantID string
}

func encodeItem(sid, pid string) string {
	return sid + ":" + pid
}

func decodeItem(s string) (string, string, error) {
	idx := strings.IndexByte(s, ':')
	if idx <= 0 || idx+1 >= len(s) {
		return "", "", fmt.Errorf("%w: no delimiter in %q", ErrInvalidItem, s)
	}
	sid := s[:idx]
	pid := s[idx+1:]
	if sid == "" || pid == "" {
		return "", "", fmt.Errorf("%w: empty sid or pid", ErrInvalidItem)
	}
	return sid, pid, nil
}

type Queue struct {
	client   *redis.Client
	key      string
	procKey  string
	deadKey  string
	depthKey string
	ctx      context.Context
}

func New(client *redis.Client, key string) *Queue {
	return &Queue{
		client:   client,
		key:      key,
		procKey:  key + ":processing",
		deadKey:  key + ":dead",
		depthKey: key + ":rl",
		ctx:      context.Background(),
	}
}

var enqueueScript = redis.NewScript(`
local key = KEYS[1]
local depth_key = KEYS[2]
local max_depth = tonumber(ARGV[1])
local payload = ARGV[2]
local participant_id = ARGV[3]
local max_queued = tonumber(ARGV[4])

local depth = redis.call("LLEN", key)
if depth >= max_depth then
    return {-1, "queue_full"}
end

local queued_count = redis.call("GET", depth_key .. ":queued:" .. participant_id)
if queued_count then
    queued_count = tonumber(queued_count)
else
    queued_count = 0
end
if queued_count >= max_queued then
    return {-2, "queued_quota"}
end

redis.call("LPUSH", key, payload)
redis.call("SET", depth_key .. ":queued:" .. participant_id, queued_count + 1)
return {0, "ok"}
`)

var claimUpdateScript = redis.NewScript(`
local proc_key = KEYS[1]
local depth_key = KEYS[2]
local payload = ARGV[1]
local participant_id = ARGV[2]
local submission_id = ARGV[3]
local max_running = tonumber(ARGV[4])
local now = ARGV[5]

local rc = redis.call("GET", depth_key .. ":running:" .. participant_id)
local running = 0
if rc then running = tonumber(rc) end

if running >= max_running then
    redis.call("LREM", proc_key, 1, payload)
    redis.call("RPUSH", KEYS[3], payload)
    return {-3, "running_quota"}
end

local qc = redis.call("GET", depth_key .. ":queued:" .. participant_id)
if qc then
    local new_qc = tonumber(qc) - 1
    if new_qc < 0 then new_qc = 0 end
    redis.call("SET", depth_key .. ":queued:" .. participant_id, new_qc)
end

redis.call("SET", depth_key .. ":running:" .. participant_id, running + 1)
redis.call("SET", depth_key .. ":running_since:" .. submission_id, now)
return {0, "ok"}
`)

var completeScript = redis.NewScript(`
local proc_key = KEYS[1]
local depth_key = KEYS[2]
local payload = ARGV[1]
local participant_id = ARGV[2]
local submission_id = ARGV[3]

redis.call("LREM", proc_key, 1, payload)

local rc = redis.call("GET", depth_key .. ":running:" .. participant_id)
if rc then
    local new_rc = tonumber(rc) - 1
    if new_rc < 0 then new_rc = 0 end
    redis.call("SET", depth_key .. ":running:" .. participant_id, new_rc)
end

redis.call("DEL", depth_key .. ":running_since:" .. submission_id)
return {0, "ok"}
`)

var failScript = redis.NewScript(`
local proc_key = KEYS[1]
local depth_key = KEYS[2]
local payload = ARGV[1]
local participant_id = ARGV[2]
local submission_id = ARGV[3]

redis.call("LREM", proc_key, 1, payload)

local rc = redis.call("GET", depth_key .. ":running:" .. participant_id)
if rc then
    local new_rc = tonumber(rc) - 1
    if new_rc < 0 then new_rc = 0 end
    redis.call("SET", depth_key .. ":running:" .. participant_id, new_rc)
end

redis.call("DEL", depth_key .. ":running_since:" .. submission_id)
return {0, "ok"}
`)

var recoverCountersScript = redis.NewScript(`
local key = KEYS[1]
local proc_key = KEYS[2]
local depth_key = KEYS[3]
local payload = ARGV[1]
local participant_id = ARGV[2]
local submission_id = ARGV[3]

redis.call("LREM", proc_key, 1, payload)
local existing = redis.call("LREM", key, 1, payload)
redis.call("LPUSH", key, payload)

local rc = redis.call("GET", depth_key .. ":running:" .. participant_id)
if rc then
    local new_rc = tonumber(rc) - 1
    if new_rc < 0 then new_rc = 0 end
    redis.call("SET", depth_key .. ":running:" .. participant_id, new_rc)
end

local qc = redis.call("GET", depth_key .. ":queued:" .. participant_id)
local new_qc = 1
if qc then
    new_qc = tonumber(qc) + 1
end
redis.call("SET", depth_key .. ":queued:" .. participant_id, new_qc)

redis.call("DEL", depth_key .. ":running_since:" .. submission_id)
return {0, "ok"}
`)

func (q *Queue) EnqueueCheck(submissionID, participantID string, maxDepth, maxQueued int) error {
	payload := encodeItem(submissionID, participantID)

	result, err := enqueueScript.Run(q.ctx, q.client,
		[]string{q.key, q.depthKey},
		maxDepth,
		payload,
		participantID,
		maxQueued,
	).Slice()

	if err != nil {
		return fmt.Errorf("enqueue script: %w", err)
	}

	code := int(result[0].(int64))
	switch code {
	case 0:
		return nil
	case -1:
		return ErrQueueFull
	case -2:
		return ErrQueuedQuota
	default:
		return fmt.Errorf("enqueue rejected (code %d)", code)
	}
}

func (q *Queue) Claim(timeout time.Duration, maxRunning int) (*JobItem, error) {
	timeoutSec := int(timeout.Seconds())
	now := fmt.Sprintf("%d", time.Now().UnixMilli())

	raw, err := q.client.BLMove(q.ctx, q.key, q.procKey, "RIGHT", "LEFT", time.Duration(timeoutSec)*time.Second).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNoJob
		}
		return nil, fmt.Errorf("claim move: %w", err)
	}

	sid, pid, err := decodeItem(raw)
	if err != nil {
		q.client.LRem(q.ctx, q.procKey, 1, raw)
		q.client.LPush(q.ctx, q.deadKey, raw)
		return nil, fmt.Errorf("%w: %s", ErrInvalidItem, err)
	}

	result, err := claimUpdateScript.Run(q.ctx, q.client,
		[]string{q.procKey, q.depthKey, q.key},
		raw,
		pid,
		sid,
		maxRunning,
		now,
	).Slice()

	if err != nil {
		q.client.LRem(q.ctx, q.procKey, 1, raw)
		q.client.RPush(q.ctx, q.key, raw)
		return nil, fmt.Errorf("claim update: %w", err)
	}

	code := int(result[0].(int64))
	switch code {
	case 0:
		return &JobItem{SubmissionID: sid, ParticipantID: pid}, nil
	case -3:
		return &JobItem{SubmissionID: sid, ParticipantID: pid}, ErrRunningQuota
	default:
		q.client.LRem(q.ctx, q.procKey, 1, raw)
		q.client.RPush(q.ctx, q.key, raw)
		return nil, fmt.Errorf("claim rejected (code %d)", code)
	}
}

func (q *Queue) Complete(submissionID, participantID string) error {
	payload := encodeItem(submissionID, participantID)

	_, err := completeScript.Run(q.ctx, q.client,
		[]string{q.procKey, q.depthKey},
		payload,
		participantID,
		submissionID,
	).Result()
	if err != nil {
		return fmt.Errorf("complete script: %w", err)
	}
	return nil
}

func (q *Queue) Fail(submissionID, participantID string) error {
	payload := encodeItem(submissionID, participantID)

	_, err := failScript.Run(q.ctx, q.client,
		[]string{q.procKey, q.depthKey},
		payload,
		participantID,
		submissionID,
	).Result()
	if err != nil {
		return fmt.Errorf("fail script: %w", err)
	}
	return nil
}

func (q *Queue) Depth() (int64, error) {
	return q.client.LLen(q.ctx, q.key).Result()
}

func (q *Queue) ProcessingLen() (int64, error) {
	return q.client.LLen(q.ctx, q.procKey).Result()
}

func (q *Queue) DeadLen() (int64, error) {
	return q.client.LLen(q.ctx, q.deadKey).Result()
}

func (q *Queue) RecoverStale(maxAge time.Duration) ([]JobItem, error) {
	now := time.Now().UnixMilli()
	thresholdMs := maxAge.Milliseconds()

	jobs, err := q.client.LRange(q.ctx, q.procKey, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("list processing: %w", err)
	}

	var recovered []JobItem
	for _, payload := range jobs {
		sid, pid, err := decodeItem(payload)
		if err != nil {
			q.client.LRem(q.ctx, q.procKey, 1, payload)
			q.client.LPush(q.ctx, q.deadKey, payload)
			continue
		}

		startedKey := q.depthKey + ":running_since:" + sid
		startedStr, err := q.client.Get(q.ctx, startedKey).Result()
		if err != nil {
			continue
		}

		startedMs, err := parseInt64(startedStr)
		if err != nil {
			continue
		}

		if now-startedMs <= thresholdMs {
			continue
		}

		recovered = append(recovered, JobItem{SubmissionID: sid, ParticipantID: pid})
	}

	return recovered, nil
}

func (q *Queue) RecoverOne(submissionID, participantID string) error {
	payload := encodeItem(submissionID, participantID)

	_, err := recoverCountersScript.Run(q.ctx, q.client,
		[]string{q.key, q.procKey, q.depthKey},
		payload,
		participantID,
		submissionID,
	).Result()
	if err != nil {
		return fmt.Errorf("recover counters: %w", err)
	}
	return nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
