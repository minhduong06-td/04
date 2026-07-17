package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
)

var (
	ErrGlobalCapacity         = errors.New("global active submission capacity reached")
	ErrParticipantQueuedQuota = errors.New("participant queued submission quota reached")
)

type StartResult string

const (
	StartStarted             StartResult = "started"
	StartQuotaBusy           StartResult = "quota_busy"
	StartDuplicateOrTerminal StartResult = "duplicate_or_terminal"
	StartNotFound            StartResult = "not_found"
)

type OutboxEvent struct {
	ID                                     int64
	SubmissionID, ParticipantID, EventType string
	AttemptCount                           int
}

type DB struct {
	*sql.DB
}

func Connect(databaseURL string) (*DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database open: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database ping: %w", err)
	}

	return &DB{db}, nil
}

func (db *DB) RunMigrations(ctx context.Context) error {
	data, err := os.ReadFile("migrations/001_init.sql")
	if err != nil {
		return fmt.Errorf("read migration file: %w", err)
	}

	if _, err := db.ExecContext(ctx, string(data)); err != nil {
		return fmt.Errorf("migration: %w", err)
	}
	return nil
}

func (db *DB) Close() error {
	return db.DB.Close()
}

type ParticipantRow struct {
	ID         string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

func (db *DB) UpsertParticipant(ctx context.Context, id string) (*ParticipantRow, error) {
	row := db.QueryRowContext(ctx,
		`INSERT INTO participants (id, created_at, last_seen_at)
		 VALUES ($1, NOW(), NOW())
		 ON CONFLICT (id) DO UPDATE SET last_seen_at = NOW()
		 RETURNING id, created_at, last_seen_at`,
		id,
	)
	var p ParticipantRow
	if err := row.Scan(&p.ID, &p.CreatedAt, &p.LastSeenAt); err != nil {
		return nil, fmt.Errorf("upsert participant: %w", err)
	}
	return &p, nil
}

type SubmissionRow struct {
	ID               string
	ParticipantID    string
	OriginalFilename *string
	SourceStorageKey string
	SourceSize       int64
	SourceSHA256     string
	Status           string
	CompileSuccess   *bool
	CompileStderr    *string
	ExitCode         *int
	Stdout           *string
	Stderr           *string
	ResultTruncated  bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

func (db *DB) CreateSubmission(ctx context.Context, id, participantID, originalFilename, sourceStorageKey string, sourceSize int64, sourceSHA256 string) (*SubmissionRow, error) {
	var origFN *string
	if originalFilename != "" {
		origFN = &originalFilename
	}

	row := db.QueryRowContext(ctx,
		`INSERT INTO submissions
		 (id, participant_id, original_filename, source_storage_key, source_size, source_sha256, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'queued')
		 RETURNING id, participant_id, original_filename, source_storage_key, source_size,
		           source_sha256, status, compile_success, compile_stderr, exit_code,
		           stdout, stderr, result_truncated, created_at, updated_at, started_at, finished_at`,
		id, participantID, origFN, sourceStorageKey, sourceSize, sourceSHA256,
	)
	return scanSubmission(row)
}

// CreateSubmissionQueued atomically enforces database-authoritative capacity
// and queued quotas, creates the queued submission, and records its enqueue
// outbox event.
func (db *DB) CreateSubmissionQueued(ctx context.Context, id, participantID, originalFilename, sourceStorageKey string, sourceSize int64, sourceSHA256 string, maxActive, maxQueued int) (*SubmissionRow, error) {
	if maxActive <= 0 || maxQueued <= 0 {
		return nil, fmt.Errorf("invalid queue limits")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin submission transaction: %w", err)
	}
	defer tx.Rollback()

	// All creators take locks in the same participant-then-global order.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('participant:' || $1, 0))`, participantID); err != nil {
		return nil, fmt.Errorf("lock participant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(520202401)`); err != nil {
		return nil, fmt.Errorf("lock global capacity: %w", err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions WHERE status IN ('queued','mock_processing')`).Scan(&count); err != nil {
		return nil, fmt.Errorf("count global active: %w", err)
	}
	if count >= maxActive {
		return nil, ErrGlobalCapacity
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions WHERE participant_id=$1 AND status='queued'`, participantID).Scan(&count); err != nil {
		return nil, fmt.Errorf("count participant queued: %w", err)
	}
	if count >= maxQueued {
		return nil, ErrParticipantQueuedQuota
	}

	var origFN *string
	if originalFilename != "" {
		origFN = &originalFilename
	}
	row := tx.QueryRowContext(ctx,
		`INSERT INTO submissions
		 (id, participant_id, original_filename, source_storage_key, source_size, source_sha256, status)
		 VALUES ($1,$2,$3,$4,$5,$6,'queued')
		 RETURNING id, participant_id, original_filename, source_storage_key, source_size,
		 source_sha256, status, compile_success, compile_stderr, exit_code, stdout, stderr,
		 result_truncated, created_at, updated_at, started_at, finished_at`,
		id, participantID, origFN, sourceStorageKey, sourceSize, sourceSHA256)
	s, err := scanSubmission(row)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO submission_outbox(submission_id,event_type) VALUES($1,'enqueue_submission')`, id); err != nil {
		return nil, fmt.Errorf("insert enqueue outbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit submission transaction: %w", err)
	}
	return s, nil
}

func (db *DB) GetSubmission(ctx context.Context, id, participantID string) (*SubmissionRow, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, participant_id, original_filename, source_storage_key, source_size,
		        source_sha256, status, compile_success, compile_stderr, exit_code,
		        stdout, stderr, result_truncated, created_at, updated_at, started_at, finished_at
		 FROM submissions
		 WHERE id = $1 AND participant_id = $2`,
		id, participantID,
	)
	return scanSubmission(row)
}

func (db *DB) StartSubmissionConditional(ctx context.Context, id string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE submissions SET status = 'mock_processing', started_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND status = 'queued'`,
		id,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (db *DB) TryStartSubmission(ctx context.Context, id string, maxRunning int) (StartResult, error) {
	if maxRunning <= 0 {
		return "", fmt.Errorf("invalid max running")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var participantID, status string
	err = tx.QueryRowContext(ctx, `SELECT participant_id,status FROM submissions WHERE id=$1`, id).Scan(&participantID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return StartNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("read submission for start: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('participant:' || $1, 0))`, participantID); err != nil {
		return "", fmt.Errorf("lock participant start: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT status FROM submissions WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		return "", err
	}
	if status != "queued" {
		return StartDuplicateOrTerminal, nil
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions WHERE participant_id=$1 AND status='mock_processing'`, participantID).Scan(&running); err != nil {
		return "", err
	}
	if running >= maxRunning {
		return StartQuotaBusy, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE submissions SET status='mock_processing',started_at=NOW(),updated_at=NOW() WHERE id=$1 AND status='queued'`, id)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		if err != nil {
			return "", err
		}
		return StartDuplicateOrTerminal, nil
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return StartStarted, nil
}

func (db *DB) FinishSubmissionConditional(ctx context.Context, id string, compileSuccess bool, compileStderr, stdout, stderr string, exitCode int, truncated bool) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE submissions SET
			status = 'finished',
			compile_success = $1,
			compile_stderr = $2,
			exit_code = $3,
			stdout = $4,
			stderr = $5,
			result_truncated = $6,
			finished_at = NOW(),
			updated_at = NOW()
		 WHERE id = $7 AND status = 'mock_processing'`,
		compileSuccess, compileStderr, exitCode, stdout, stderr, truncated, id,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (db *DB) RecoverToQueued(ctx context.Context, id string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE submissions SET status = 'queued', started_at = NULL, updated_at = NOW()
		 WHERE id = $1 AND status = 'mock_processing'`,
		id,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (db *DB) RecoverStaleSubmission(ctx context.Context, id string, staleBefore time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE submissions SET status='queued',started_at=NULL,updated_at=NOW() WHERE id=$1 AND status='mock_processing' AND started_at <= $2`, id, staleBefore)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO submission_outbox(submission_id,event_type) VALUES($1,'enqueue_submission')`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (db *DB) ListStaleProcessing(ctx context.Context, staleBefore time.Time, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM submissions WHERE status='mock_processing' AND started_at <= $1 ORDER BY started_at LIMIT $2`, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) ProcessNextOutbox(ctx context.Context, maxBackoff time.Duration, deliver func(OutboxEvent) error) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var e OutboxEvent
	err = tx.QueryRowContext(ctx, `SELECT o.id,o.submission_id,s.participant_id,o.event_type,o.attempt_count FROM submission_outbox o JOIN submissions s ON s.id=o.submission_id WHERE o.delivered_at IS NULL AND o.available_at <= NOW() ORDER BY o.id FOR UPDATE OF o SKIP LOCKED LIMIT 1`).Scan(&e.ID, &e.SubmissionID, &e.ParticipantID, &e.EventType, &e.AttemptCount)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := deliver(e); err != nil {
		backoff := time.Second << min(e.AttemptCount, 20)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		msg := err.Error()
		if len(msg) > 512 {
			msg = msg[:512]
		}
		_, uerr := tx.ExecContext(ctx, `UPDATE submission_outbox SET attempt_count=attempt_count+1,last_error=$2,available_at=NOW()+$3::interval WHERE id=$1`, e.ID, msg, fmt.Sprintf("%f seconds", backoff.Seconds()))
		if uerr != nil {
			return true, uerr
		}
		return true, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_outbox SET delivered_at=NOW(),last_error=NULL WHERE id=$1 AND delivered_at IS NULL`, e.ID); err != nil {
		return true, err
	}
	return true, tx.Commit()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (db *DB) SetInternalError(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE submissions SET status = 'internal_error', finished_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND status IN ('queued','mock_processing')`,
		id,
	)
	return err
}

func (db *DB) CountParticipantQueuedOrRunning(ctx context.Context, participantID string) (int, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM submissions
		 WHERE participant_id = $1 AND status IN ('queued', 'mock_processing')`,
		participantID,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func scanSubmission(row *sql.Row) (*SubmissionRow, error) {
	var s SubmissionRow
	err := row.Scan(
		&s.ID, &s.ParticipantID, &s.OriginalFilename, &s.SourceStorageKey,
		&s.SourceSize, &s.SourceSHA256, &s.Status,
		&s.CompileSuccess, &s.CompileStderr, &s.ExitCode,
		&s.Stdout, &s.Stderr, &s.ResultTruncated,
		&s.CreatedAt, &s.UpdatedAt, &s.StartedAt, &s.FinishedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan submission: %w", err)
	}
	return &s, nil
}
