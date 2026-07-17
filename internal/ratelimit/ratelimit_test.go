package ratelimit

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"hustack/internal/config"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	client := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   9,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := client.Ping(ctx).Err()
	cancel()
	if err != nil {
		client.Close()
		if os.Getenv("HUSTACK_REQUIRE_REDIS") == "1" {
			t.Fatalf("required Redis unavailable: %v", err)
		}
		t.Skipf("Redis unavailable: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	err = client.FlushDB(ctx).Err()
	cancel()
	if err != nil {
		client.Close()
		t.Fatalf("flush isolated Redis test database before test: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := client.FlushDB(ctx).Err(); err != nil {
			t.Errorf("flush isolated Redis test database after test: %v", err)
		}
		if err := client.Close(); err != nil {
			t.Errorf("close Redis test client: %v", err)
		}
	})
	return client
}

func testRL(t *testing.T) *RateLimiter {
	t.Helper()
	client := testRedisClient(t)
	cfg := &config.Config{
		SubmitRatePerMinute:   6,
		SubmitRatePerHour:     120,
		IPSubmitRatePerMinute: 20,
		PollRatePerMinute:     60,
	}
	rl := New(client, cfg)
	return rl
}

func testRLWithClock(t *testing.T) (*RateLimiter, *fakeClock) {
	t.Helper()
	client := testRedisClient(t)
	cfg := &config.Config{
		SubmitRatePerMinute:   6,
		SubmitRatePerHour:     120,
		IPSubmitRatePerMinute: 20,
		PollRatePerMinute:     60,
	}
	rl := New(client, cfg)
	fc := &fakeClock{now: time.Now()}
	rl.SetClock(fc)
	return rl, fc
}

func TestParticipantSubmitRateLimit(t *testing.T) {
	rl := testRL(t)
	pid := "test-participant-1"

	for i := 0; i < 6; i++ {
		allowed, _, err := rl.AllowParticipantSubmit(pid)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("iteration %d: expected allowed", i)
		}
	}

	allowed, retryAfter, err := rl.AllowParticipantSubmit(pid)
	if err != nil {
		t.Fatalf("final check: %v", err)
	}
	if allowed {
		t.Fatal("expected rate limited after 6 requests")
	}
	if retryAfter <= 0 {
		t.Fatal("expected positive retry-after")
	}
}

func TestIPSubmitRateLimit(t *testing.T) {
	rl := testRL(t)
	ip := "192.168.1.1"

	for i := 0; i < 20; i++ {
		allowed, _, err := rl.AllowIPSubmit(ip)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("iteration %d: expected allowed", i)
		}
	}

	allowed, retryAfter, err := rl.AllowIPSubmit(ip)
	if err != nil {
		t.Fatalf("final check: %v", err)
	}
	if allowed {
		t.Fatal("expected rate limited")
	}
	if retryAfter <= 0 {
		t.Fatal("expected positive retry-after")
	}
}

func TestTokenRefill(t *testing.T) {
	rl, clock := testRLWithClock(t)
	pid := "test-refill"

	for i := 0; i < 6; i++ {
		allowed, _, err := rl.AllowParticipantSubmit(pid)
		if err != nil {
			t.Fatalf("consume %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("consume %d: expected allowed", i)
		}
	}

	allowed, _, _ := rl.AllowParticipantSubmit(pid)
	if allowed {
		t.Fatal("expected exhausted")
	}

	clock.Advance(70 * time.Second)

	allowed, _, err := rl.AllowParticipantSubmit(pid)
	if err != nil {
		t.Fatalf("after refill: %v", err)
	}
	if !allowed {
		t.Fatal("expected allowed after refill")
	}
}

func TestDifferentParticipantsDontAffect(t *testing.T) {
	rl := testRL(t)

	allowed1, _, _ := rl.AllowParticipantSubmit("user-a")
	allowed2, _, _ := rl.AllowParticipantSubmit("user-b")
	if !allowed1 || !allowed2 {
		t.Fatal("both should be allowed")
	}
}

func TestNoRaceOnRefresh(t *testing.T) {
	rl := testRL(t)
	pid := "test-race"

	for i := 0; i < 10; i++ {
		rl.AllowParticipantSubmit(pid)
	}
}

func TestPollRateLimit(t *testing.T) {
	rl := testRL(t)
	pid := "test-poll"

	for i := 0; i < 60; i++ {
		allowed, _, err := rl.AllowPoll(pid)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("iteration %d: expected allowed", i)
		}
	}

	allowed, _, err := rl.AllowPoll(pid)
	if err != nil {
		t.Fatalf("final check: %v", err)
	}
	if allowed {
		t.Fatal("expected rate limited after 60 polls")
	}
}
