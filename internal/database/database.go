package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
)

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
