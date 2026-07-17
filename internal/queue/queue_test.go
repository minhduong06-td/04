package queue

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testQueue(t *testing.T, consumer string) (*Queue, *redis.Client) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	c := redis.NewClient(&redis.Options{Addr: addr, DB: 1})
	if err := c.Ping(context.Background()).Err(); err != nil {
		if os.Getenv("HUSTACK_REQUIRE_REDIS") == "1" {
			t.Fatalf("required Redis unavailable: %v", err)
		}
		t.Skip("Redis unavailable")
	}
	key := "test:stream:" + t.Name()
	q := NewStream(c, key, "g", consumer, time.Second)
	if err := q.EnsureConsumerGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Del(context.Background(), key); c.Close() })
	return q, c
}

func TestFIFOAndAck(t *testing.T) {
	q, _ := testQueue(t, "c1")
	ctx := context.Background()
	for _, id := range []string{"A", "B", "C"} {
		if _, e := q.Enqueue(ctx, id, "p"); e != nil {
			t.Fatal(e)
		}
	}
	for _, want := range []string{"A", "B", "C"} {
		j, e := q.Claim(ctx, time.Second)
		if e != nil || j.SubmissionID != want {
			t.Fatalf("want %s got %#v %v", want, j, e)
		}
		if e = q.Ack(ctx, j.StreamID); e != nil {
			t.Fatal(e)
		}
		if e = q.Ack(ctx, j.StreamID); e != nil {
			t.Fatal(e)
		}
	}
}
func TestGroupIdempotentAndDuplicate(t *testing.T) {
	q, _ := testQueue(t, "c1")
	if err := q.EnsureConsumerGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	q.Enqueue(context.Background(), "A", "p")
	q.Enqueue(context.Background(), "A", "p")
	j1, _ := q.Claim(context.Background(), time.Second)
	q.Ack(context.Background(), j1.StreamID)
	j2, _ := q.Claim(context.Background(), time.Second)
	if j2.SubmissionID != "A" {
		t.Fatal("duplicate not observable")
	}
}
func TestMalformedDiscarded(t *testing.T) {
	q, c := testQueue(t, "c1")
	c.XAdd(context.Background(), &redis.XAddArgs{Stream: q.key, Values: map[string]any{"submission_id": "A"}})
	_, err := q.Claim(context.Background(), time.Second)
	if !errors.Is(err, ErrInvalidItem) {
		t.Fatalf("got %v", err)
	}
	if n, _ := c.XLen(context.Background(), q.key).Result(); n != 0 {
		t.Fatalf("malformed retained: %d", n)
	}
}
func TestAutoClaimStale(t *testing.T) {
	q, _ := testQueue(t, "c1")
	q.Enqueue(context.Background(), "A", "p")
	j, _ := q.Claim(context.Background(), time.Second)
	q2 := NewStream(q.client, q.key, q.group, "c2", time.Second)
	if _, e := q2.ClaimStale(context.Background(), time.Hour); !errors.Is(e, ErrNoJob) {
		t.Fatalf("non-stale: %v", e)
	}
	time.Sleep(20 * time.Millisecond)
	got, e := q2.ClaimStale(context.Background(), time.Millisecond)
	if e != nil || got.StreamID != j.StreamID {
		t.Fatalf("reclaim %#v %v", got, e)
	}
}
func TestTwoConsumersNewEntryOnce(t *testing.T) {
	q, _ := testQueue(t, "c1")
	q2 := NewStream(q.client, q.key, q.group, "c2", time.Second)
	q.Enqueue(context.Background(), "A", "p")
	ch := make(chan *JobItem, 2)
	for _, x := range []*Queue{q, q2} {
		go func(x *Queue) { j, _ := x.Claim(context.Background(), 100*time.Millisecond); ch <- j }(x)
	}
	a, b := <-ch, <-ch
	if (a == nil) == (b == nil) {
		t.Fatalf("expected exactly one delivery: %#v %#v", a, b)
	}
}
func TestWrongType(t *testing.T) {
	q, c := testQueue(t, "c1")
	c.Del(context.Background(), q.key)
	c.Set(context.Background(), q.key, "keep", 0)
	if _, e := q.Enqueue(context.Background(), "A", "p"); e == nil {
		t.Fatal("expected wrong-type error")
	}
	if v, _ := c.Get(context.Background(), q.key).Result(); v != "keep" {
		t.Fatal("unrelated data changed")
	}
}
