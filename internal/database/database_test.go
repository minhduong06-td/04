package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		u = "postgres://hustack:hustack@127.0.0.1:15432/hustack?sslmode=disable"
	}
	db, e := Connect(u)
	if e != nil {
		if os.Getenv("HUSTACK_REQUIRE_POSTGRES") == "1" {
			t.Fatalf("required PostgreSQL unavailable: %v", e)
		}
		t.Skip("PostgreSQL unavailable")
	}
	t.Cleanup(func() { db.Close() })
	if _, e = db.Exec(`TRUNCATE submission_outbox,submissions,participants RESTART IDENTITY CASCADE`); e != nil {
		t.Fatal(e)
	}
	return db
}
func participant(t *testing.T, db *DB, id string) {
	t.Helper()
	if _, e := db.UpsertParticipant(context.Background(), id); e != nil {
		t.Fatal(e)
	}
}
func create(db *DB, id, p string, maxA, maxQ int) (*SubmissionRow, error) {
	return db.CreateSubmissionQueued(context.Background(), id, p, "", id+".c", 1, "sha", maxA, maxQ)
}

func TestConcurrentQueuedQuotaAndAtomicOutbox(t *testing.T) {
	db := testDB(t)
	participant(t, db, "p")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, e := create(db, fmt.Sprintf("s%d", i), "p", 10, 1); errs <- e }(i)
	}
	wg.Wait()
	close(errs)
	ok, quota := 0, 0
	for e := range errs {
		if e == nil {
			ok++
		} else if errors.Is(e, ErrParticipantQueuedQuota) {
			quota++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || quota != 1 {
		t.Fatalf("ok=%d quota=%d", ok, quota)
	}
	var subs, out int
	db.QueryRow(`SELECT COUNT(*) FROM submissions`).Scan(&subs)
	db.QueryRow(`SELECT COUNT(*) FROM submission_outbox`).Scan(&out)
	if subs != 1 || out != 1 {
		t.Fatalf("partial state submissions=%d outbox=%d", subs, out)
	}
}

func TestGlobalCapacity(t *testing.T) {
	db := testDB(t)
	participant(t, db, "p1")
	participant(t, db, "p2")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, p := range []string{"p1", "p2"} {
		wg.Add(1)
		go func(i int, p string) { defer wg.Done(); _, e := create(db, fmt.Sprintf("g%d", i), p, 1, 3); errs <- e }(i, p)
	}
	wg.Wait()
	close(errs)
	ok, full := 0, 0
	for e := range errs {
		if e == nil {
			ok++
		} else if errors.Is(e, ErrGlobalCapacity) {
			full++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || full != 1 {
		t.Fatalf("ok=%d full=%d", ok, full)
	}
}

func TestTryStartFinishAndRecovery(t *testing.T) {
	db := testDB(t)
	participant(t, db, "p")
	create(db, "a", "p", 10, 3)
	create(db, "b", "p", 10, 3)
	var wg sync.WaitGroup
	results := make(chan StartResult, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r, e := db.TryStartSubmission(context.Background(), id, 1)
			if e != nil {
				t.Error(e)
			}
			results <- r
		}(id)
	}
	wg.Wait()
	close(results)
	started, busy := 0, 0
	for r := range results {
		if r == StartStarted {
			started++
		}
		if r == StartQuotaBusy {
			busy++
		}
	}
	if started != 1 || busy != 1 {
		t.Fatalf("started=%d busy=%d", started, busy)
	}
	var id string
	db.QueryRow(`SELECT id FROM submissions WHERE status='mock_processing'`).Scan(&id)
	if r, _ := db.TryStartSubmission(context.Background(), id, 1); r != StartDuplicateOrTerminal {
		t.Fatalf("duplicate start %s", r)
	}
	ok, e := db.FinishSubmissionConditional(context.Background(), id, true, "", "one", "", 0, false)
	if e != nil || !ok {
		t.Fatalf("finish %v %v", ok, e)
	}
	ok, _ = db.FinishSubmissionConditional(context.Background(), id, false, "changed", "changed", "changed", 1, true)
	if ok {
		t.Fatal("duplicate finish updated")
	}
	var stdout string
	db.QueryRow(`SELECT stdout FROM submissions WHERE id=$1`, id).Scan(&stdout)
	if stdout != "one" {
		t.Fatal("terminal data changed")
	}
	var other string
	db.QueryRow(`SELECT id FROM submissions WHERE status='queued'`).Scan(&other)
	db.TryStartSubmission(context.Background(), other, 1)
	db.Exec(`UPDATE submissions SET started_at=NOW()-INTERVAL '1 hour' WHERE id=$1`, other)
	before := time.Now().Add(-time.Minute)
	recovered, e := db.RecoverStaleSubmission(context.Background(), other, before)
	if e != nil || !recovered {
		t.Fatalf("recover %v %v", recovered, e)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM submission_outbox WHERE submission_id=$1`, other).Scan(&n)
	if n != 2 {
		t.Fatalf("outbox count=%d", n)
	}
	recovered, _ = db.RecoverStaleSubmission(context.Background(), other, before)
	if recovered {
		t.Fatal("duplicate recovery")
	}
}
