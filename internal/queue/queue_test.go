package queue

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr, DB: 1})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not available at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})
	return client
}

func TestFIFO(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:fifo")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	if err := q.EnqueueCheck("A", "user1", 100, 10); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if err := q.EnqueueCheck("B", "user1", 100, 10); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	if err := q.EnqueueCheck("C", "user1", 100, 10); err != nil {
		t.Fatalf("enqueue C: %v", err)
	}

	job1, err := q.Claim(time.Second, 10)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if job1.SubmissionID != "A" {
		t.Fatalf("expected A, got %s", job1.SubmissionID)
	}
	q.Complete(job1.SubmissionID, job1.ParticipantID)

	job2, err := q.Claim(time.Second, 10)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if job2.SubmissionID != "B" {
		t.Fatalf("expected B, got %s", job2.SubmissionID)
	}
	q.Complete(job2.SubmissionID, job2.ParticipantID)

	job3, err := q.Claim(time.Second, 10)
	if err != nil {
		t.Fatalf("claim 3: %v", err)
	}
	if job3.SubmissionID != "C" {
		t.Fatalf("expected C, got %s", job3.SubmissionID)
	}
	q.Complete(job3.SubmissionID, job3.ParticipantID)
}

func TestGlobalMaxDepth(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:maxdepth")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	if err := q.EnqueueCheck("A", "user1", 2, 10); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if err := q.EnqueueCheck("B", "user1", 2, 10); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	if err := q.EnqueueCheck("C", "user2", 2, 10); err != ErrQueueFull {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
}

func TestQueuedQuota(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:queuedquota")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	if err := q.EnqueueCheck("A", "user1", 100, 2); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if err := q.EnqueueCheck("B", "user1", 100, 2); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	if err := q.EnqueueCheck("C", "user1", 100, 2); err != ErrQueuedQuota {
		t.Fatalf("expected ErrQueuedQuota, got %v", err)
	}
	// Different user should work
	if err := q.EnqueueCheck("D", "user2", 100, 2); err != nil {
		t.Fatalf("enqueue D user2: %v", err)
	}
}

func TestRunningQuotaAtClaim(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:runquota")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	if err := q.EnqueueCheck("A", "user1", 100, 10); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if err := q.EnqueueCheck("B", "user1", 100, 10); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}

	// First claim succeeds
	job1, err := q.Claim(time.Second, 1)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if job1.SubmissionID != "A" {
		t.Fatalf("expected A, got %s", job1.SubmissionID)
	}

	// Second claim for same participant should block by running quota
	job2, err := q.Claim(time.Second, 1)
	if err != ErrRunningQuota {
		t.Fatalf("expected ErrRunningQuota, got %v (job: %s)", err, job2)
	}
	if job2 == nil {
		t.Fatal("expected job2 to be non-nil (returned on running quota)")
	}
	if job2.SubmissionID != "B" {
		t.Fatalf("expected B returned, got %s", job2.SubmissionID)
	}

	// Complete first
	q.Complete(job1.SubmissionID, job1.ParticipantID)

	// Now second should claim successfully
	job2b, err := q.Claim(time.Second, 1)
	if err != nil {
		t.Fatalf("claim 2b: %v", err)
	}
	if job2b.SubmissionID != "B" {
		t.Fatalf("expected B, got %s", job2b.SubmissionID)
	}
	q.Complete(job2b.SubmissionID, job2b.ParticipantID)
}

func TestTwoWorkersSameParticipant(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:twoworkers")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	// Enqueue 4 jobs for same participant
	for i := 0; i < 4; i++ {
		sid := string(rune('A' + i))
		if err := q.EnqueueCheck(sid, "user1", 100, 10); err != nil {
			t.Fatalf("enqueue %s: %v", sid, err)
		}
	}

	// Two parallel claim goroutines with maxRunning=1
	type claimResult struct {
		item *JobItem
		err  error
	}
	ch := make(chan claimResult, 4)

	for i := 0; i < 4; i++ {
		go func() {
			item, err := q.Claim(5*time.Second, 1)
			ch <- claimResult{item, err}
		}()
	}

	seen := make(map[string]int)
	for i := 0; i < 4; i++ {
		r := <-ch
		if r.err == ErrRunningQuota {
			// Item returned to queue, retry claim
			go func() {
				item, err := q.Claim(5*time.Second, 1)
				ch <- claimResult{item, err}
			}()
			continue
		}
		if r.err != nil {
			t.Fatalf("unexpected claim error: %v", r.err)
		}
		seen[r.item.SubmissionID]++
		// Simulate work then complete
		q.Complete(r.item.SubmissionID, r.item.ParticipantID)
	}

	// Each should be seen exactly once
	for _, sid := range []string{"A", "B", "C", "D"} {
		if seen[sid] != 1 {
			t.Errorf("expected 1 claim for %s, got %d", sid, seen[sid])
		}
	}
}

func TestCountersEnqueueClaimComplete(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:ctr_ok")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	q.EnqueueCheck("A", "user1", 100, 10)
	q.EnqueueCheck("B", "user1", 100, 10)

	queuedKey := q.depthKey + ":queued:user1"
	runningKey := q.depthKey + ":running:user1"

	if v, _ := q.client.Get(q.ctx, queuedKey).Int(); v != 2 {
		t.Fatalf("expected queued=2, got %d", v)
	}
	if v, _ := q.client.Get(q.ctx, runningKey).Int(); v != 0 {
		t.Fatalf("expected running=0, got %d", v)
	}

	job1, _ := q.Claim(time.Second, 10)
	if v, _ := q.client.Get(q.ctx, queuedKey).Int(); v != 1 {
		t.Fatalf("expected queued=1 after claim, got %d", v)
	}
	if v, _ := q.client.Get(q.ctx, runningKey).Int(); v != 1 {
		t.Fatalf("expected running=1 after claim, got %d", v)
	}

	q.Complete(job1.SubmissionID, job1.ParticipantID)
	if v, _ := q.client.Get(q.ctx, queuedKey).Int(); v != 1 {
		t.Fatalf("expected queued=1 after complete, got %d", v)
	}
	if v, _ := q.client.Get(q.ctx, runningKey).Int(); v != 0 {
		t.Fatalf("expected running=0 after complete, got %d", v)
	}

	job2, _ := q.Claim(time.Second, 10)
	q.Fail(job2.SubmissionID, job2.ParticipantID)
	if v, _ := q.client.Get(q.ctx, runningKey).Int(); v != 0 {
		t.Fatalf("expected running=0 after fail, got %d", v)
	}
}

func TestDuplicateComplete(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:dupcomp")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	q.EnqueueCheck("A", "user1", 100, 10)
	job, _ := q.Claim(time.Second, 10)

	if err := q.Complete(job.SubmissionID, job.ParticipantID); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if err := q.Complete(job.SubmissionID, job.ParticipantID); err != nil {
		t.Fatalf("duplicate complete: %v", err)
	}
	// Running counter should stay at 0
	if v, _ := q.client.Get(q.ctx, q.depthKey+":running:user1").Int(); v != 0 {
		t.Fatalf("expected running=0 after dup complete, got %d", v)
	}
}

func TestDuplicateFail(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:dupfail")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	q.EnqueueCheck("A", "user1", 100, 10)
	job, _ := q.Claim(time.Second, 10)

	if err := q.Fail(job.SubmissionID, job.ParticipantID); err != nil {
		t.Fatalf("first fail: %v", err)
	}
	if err := q.Fail(job.SubmissionID, job.ParticipantID); err != nil {
		t.Fatalf("duplicate fail: %v", err)
	}
}

func TestStaleRecovery(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:stale")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	q.EnqueueCheck("A", "user1", 100, 10)
	q.EnqueueCheck("B", "user1", 100, 10)

	// Claim A
	q.Claim(time.Second, 10)

	// Set running_since to old timestamp to simulate stale
	oldTs := fmt.Sprintf("%d", time.Now().UnixMilli()-10*60*1000)
	q.client.Set(q.ctx, q.depthKey+":running_since:A", oldTs, 0)

	// Recover should find A as stale
	recovered, err := q.RecoverStale(time.Minute)
	if err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered, got %d", len(recovered))
	}
	if recovered[0].SubmissionID != "A" {
		t.Fatalf("expected A, got %s", recovered[0].SubmissionID)
	}
}

func TestNoNegativeCounters(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:negctr")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	// Over-complete to test negative prevention
	if err := q.Complete("nonexistent", "user1"); err != nil {
		t.Fatalf("complete nonexistent: %v", err)
	}
	if v, _ := q.client.Get(q.ctx, q.depthKey+":running:user1").Int(); v < 0 {
		t.Fatalf("negative running counter: %d", v)
	}

	if err := q.Fail("nonexistent2", "user1"); err != nil {
		t.Fatalf("fail nonexistent: %v", err)
	}
	if v, _ := q.client.Get(q.ctx, q.depthKey+":running:user1").Int(); v < 0 {
		t.Fatalf("negative running counter after fail: %d", v)
	}
}

func TestDeadLetter(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:dead")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	// Push a corrupted item directly onto processing queue
	q.client.LPush(q.ctx, q.procKey, "corrupted-no-delimiter")

	// RecoverStale scans procKey and should move corrupted to dead-letter
	recovered, err := q.RecoverStale(time.Minute)
	if err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("expected 0 recovered, got %d", len(recovered))
	}

	// Verify corrupted item went to dead-letter
	if n, _ := q.DeadLen(); n != 1 {
		t.Fatalf("expected 1 dead-letter item, got %d", n)
	}

	// Verify processing queue is empty
	if n, _ := q.ProcessingLen(); n != 0 {
		t.Fatalf("expected 0 processing items, got %d", n)
	}
}

func TestRecoverOne(t *testing.T) {
	client := redisClient(t)
	q := New(client, "test:recoverone")
	defer q.client.Del(q.ctx, q.key, q.procKey, q.deadKey, q.depthKey+":queued:*", q.depthKey+":running:*")

	q.EnqueueCheck("A", "user1", 100, 10)
	job, _ := q.Claim(time.Second, 10)

	// Simulate recovery
	if err := q.RecoverOne(job.SubmissionID, job.ParticipantID); err != nil {
		t.Fatalf("RecoverOne: %v", err)
	}

	// Check counters: running decreased, queued increased
	if v, _ := q.client.Get(q.ctx, q.depthKey+":running:user1").Int(); v != 0 {
		t.Fatalf("expected running=0 after recover, got %d", v)
	}
	if v, _ := q.client.Get(q.ctx, q.depthKey+":queued:user1").Int(); v != 1 {
		t.Fatalf("expected queued=1 after recover, got %d", v)
	}

	// Job should be back in queue
	reclaimed, err := q.Claim(time.Second, 10)
	if err != nil {
		t.Fatalf("reclaim after recover: %v", err)
	}
	if reclaimed.SubmissionID != "A" {
		t.Fatalf("expected A, got %s", reclaimed.SubmissionID)
	}
	q.Complete(reclaimed.SubmissionID, reclaimed.ParticipantID)
}
