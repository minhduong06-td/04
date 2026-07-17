package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrNoJob       = errors.New("no job available")
	ErrInvalidItem = errors.New("invalid stream item")
)

type InvalidItemError struct {
	StreamID string
	Reason   string
}

func (e *InvalidItemError) Error() string {
	return fmt.Sprintf("%v %s: %s", ErrInvalidItem, e.StreamID, e.Reason)
}
func (e *InvalidItemError) Unwrap() error { return ErrInvalidItem }

type JobItem struct {
	StreamID      string
	SubmissionID  string
	ParticipantID string
}

type Queue struct {
	client               *redis.Client
	key, group, consumer string
	opTimeout            time.Duration
}

func New(client *redis.Client, key string) *Queue {
	return NewStream(client, key, "mock-workers", "worker-1", 2*time.Second)
}

func NewStream(client *redis.Client, key, group, consumer string, opTimeout time.Duration) *Queue {
	return &Queue{client: client, key: key, group: group, consumer: consumer, opTimeout: opTimeout}
}

func (q *Queue) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, q.opTimeout)
}

func (q *Queue) EnsureConsumerGroup(parent context.Context) error {
	ctx, cancel := q.operationContext(parent)
	defer cancel()
	err := q.client.XGroupCreateMkStream(ctx, q.key, q.group, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return fmt.Errorf("create consumer group: %w", err)
	}
	return nil
}

func isBusyGroup(err error) bool {
	return err != nil && len(err.Error()) >= 9 && err.Error()[:9] == "BUSYGROUP"
}

func (q *Queue) Enqueue(parent context.Context, submissionID, participantID string) (string, error) {
	if submissionID == "" || participantID == "" {
		return "", ErrInvalidItem
	}
	ctx, cancel := q.operationContext(parent)
	defer cancel()
	id, err := q.client.XAdd(ctx, &redis.XAddArgs{Stream: q.key, Values: map[string]any{"submission_id": submissionID, "participant_id": participantID}}).Result()
	if err != nil {
		return "", fmt.Errorf("xadd queue: %w", err)
	}
	return id, nil
}

func (q *Queue) Claim(parent context.Context, block time.Duration) (*JobItem, error) {
	ctx, cancel := context.WithTimeout(parent, block+q.opTimeout)
	defer cancel()
	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: q.group, Consumer: q.consumer, Streams: []string{q.key, ">"}, Count: 1, Block: block}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("xreadgroup: %w", err)
	}
	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return nil, ErrNoJob
	}
	return q.parseOrDiscard(parent, streams[0].Messages[0])
}

func (q *Queue) ClaimStale(parent context.Context, minIdle time.Duration) (*JobItem, error) {
	ctx, cancel := q.operationContext(parent)
	defer cancel()
	msgs, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: q.key, Group: q.group, Consumer: q.consumer, MinIdle: minIdle, Start: "0-0", Count: 1}).Result()
	if errors.Is(err, redis.Nil) || (err == nil && len(msgs) == 0) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("xautoclaim: %w", err)
	}
	return q.parseOrDiscard(parent, msgs[0])
}

func stringField(values map[string]any, key string) (string, bool) {
	v, ok := values[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

func (q *Queue) parseOrDiscard(parent context.Context, m redis.XMessage) (*JobItem, error) {
	sid, sok := stringField(m.Values, "submission_id")
	pid, pok := stringField(m.Values, "participant_id")
	if !sok || !pok {
		if err := q.Ack(parent, m.ID); err != nil {
			return nil, fmt.Errorf("discard malformed %s: %w", m.ID, err)
		}
		return nil, &InvalidItemError{StreamID: m.ID, Reason: "missing submission_id or participant_id"}
	}
	return &JobItem{StreamID: m.ID, SubmissionID: sid, ParticipantID: pid}, nil
}

func (q *Queue) Ack(parent context.Context, streamID string) error {
	ctx, cancel := q.operationContext(parent)
	defer cancel()
	if _, err := q.client.XAck(ctx, q.key, q.group, streamID).Result(); err != nil {
		return fmt.Errorf("xack: %w", err)
	}
	if _, err := q.client.XDel(ctx, q.key, streamID).Result(); err != nil {
		return fmt.Errorf("xdel: %w", err)
	}
	return nil
}

func (q *Queue) Depth(parent context.Context) (int64, error) {
	ctx, c := q.operationContext(parent)
	defer c()
	return q.client.XLen(ctx, q.key).Result()
}
