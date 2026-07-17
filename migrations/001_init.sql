-- 001_init.sql
-- Run by migration role (postgres superuser via init)
-- Creates tables and application role

CREATE TABLE IF NOT EXISTS participants (
    id          TEXT PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS submissions (
    id                  TEXT PRIMARY KEY,
    participant_id      TEXT NOT NULL REFERENCES participants(id),
    original_filename   TEXT,
    source_storage_key  TEXT NOT NULL UNIQUE,
    source_size         BIGINT NOT NULL,
    source_sha256       TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'queued'
                        CHECK (status IN ('queued','mock_processing','finished','internal_error')),
    compile_success     BOOLEAN,
    compile_stderr      TEXT,
    exit_code           INTEGER,
    stdout              TEXT,
    stderr              TEXT,
    result_truncated    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_submissions_participant_id ON submissions(participant_id);
CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(status);
CREATE INDEX IF NOT EXISTS idx_submissions_participant_status
    ON submissions(participant_id, status);
CREATE INDEX IF NOT EXISTS idx_submissions_stale_processing
    ON submissions(started_at) WHERE status = 'mock_processing';

CREATE TABLE IF NOT EXISTS submission_outbox (
    id              BIGSERIAL PRIMARY KEY,
    submission_id   TEXT NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL CHECK (event_type IN ('enqueue_submission')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    available_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ,
    attempt_count   INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error      TEXT
);

CREATE INDEX IF NOT EXISTS idx_submission_outbox_ready
    ON submission_outbox(available_at, id) WHERE delivered_at IS NULL;
