# Phase 1 Acceptance Report

## Architecture

PostgreSQL is authoritative for submission lifecycle, global active capacity,
participant queued quota, and participant running quota. Redis 7 Streams
(`hustack:queue:v2`, consumer group `mock-workers`) is an at-least-once
transport. Duplicate stream messages are expected and made safe by conditional
database transitions.

The trusted mock-worker contains three loops: transactional outbox dispatch,
stream consumption, and stale database recovery. It does not compile or execute
submitted source.

## Database and migrations

`migrations/001_init.sql` adds active/stale indexes and
`submission_outbox`. Submission creation takes participant/global transaction
advisory locks, enforces quotas, and inserts the queued row plus outbox event in
one transaction. TryStart serializes per participant and returns explicit
started/quota/duplicate/not-found outcomes. Finish and stale recovery are
conditional; recovery inserts a new outbox event atomically.

## Redis Stream and idempotency behavior

- Consumer-group creation is idempotent.
- Enqueue uses XADD with separate submission and participant fields.
- New delivery uses XREADGROUP; stale pending recovery uses XAUTOCLAIM.
- Successful/obsolete delivery uses XACK followed by XDEL.
- Malformed entries are safely acknowledged/deleted and return a typed error.
- Redis contains no lifecycle quota counters and performs no list/LREM
  compensation.

Outbox rows are selected with `FOR UPDATE SKIP LOCKED`. They are marked
delivered only after XADD succeeds. Failures retain the event, increment its
attempt count, store a bounded error, and apply bounded exponential backoff.
Response loss may duplicate XADD, which database conditional transitions make
safe.

## Stale and crash recovery

- A crash after DB creation leaves a durable undelivered outbox event.
- A crash after XADD may duplicate delivery on retry.
- A crash after TryStart leaves `mock_processing`; the stale DB loop
  conditionally returns it to queued and creates a new outbox event atomically.
- A crash after Finish but before XACK produces an obsolete duplicate that is
  acknowledged after TryStart reports terminal/duplicate.
- Pending Redis messages are reclaimed with XAUTOCLAIM.

## Commands and results

Real service gates used fresh `redis:7-alpine` and `postgres:16-alpine`
containers.

```text
HUSTACK_REQUIRE_POSTGRES=1 go test ./internal/database -count=20       PASS
HUSTACK_REQUIRE_POSTGRES=1 go test -race ./internal/database -count=10 PASS
HUSTACK_REQUIRE_REDIS=1 go test -v ./internal/queue -count=1          PASS, 0 SKIP
HUSTACK_REQUIRE_REDIS=1 go test ./internal/queue -count=20            PASS
HUSTACK_REQUIRE_REDIS=1 go test -race ./internal/queue -count=10      PASS
go vet ./...                                                           PASS
go build ./...                                                         PASS
go test ./...                                                          PASS
go test -race ./...                                                    PASS
docker compose config --quiet                                         PASS
make integration-test                                                 PASS
git diff --check                                                       PASS
```

Focused repeated executions: 60 database test executions plus 30 database race
executions; 120 Redis test executions plus 60 Redis race executions. The final
integration suite ran 24 tests. Required skipped-test count: **0**.

## Fresh image and service evidence

The acceptance sequence ran `docker compose down -v --remove-orphans`, then
`docker compose build --no-cache api mock-worker`, followed by force recreation.
Fresh image IDs reported were:

- API: `sha256:e1fa08d910649ee97f93271ad41ec71573ab088e034e9225f3b31a649b8717db`
- mock-worker: `sha256:e5e6193e45dc46764759a954179091bd7ebc844acd36d8098a04f5608e958e8d`

PostgreSQL, Redis, API, and Nginx reported healthy; mock-worker remained running.
Only Nginx publishes a host port. Readiness checks PostgreSQL, Redis, and an
actual create/write/remove operation in source storage; healthz remains
liveness-only.

Integration coverage verified textarea and `.c` submissions, immediate queued
responses, asynchronous completion, owner-only access, escaped script markup,
size limits including 10 MiB boundaries, CSRF/origin handling, and health routes.
Database concurrency tests verify participant/global quotas and one running job
per participant; rate-limit packages retain their unit coverage.

## Phase 2 limitations

Compilation, GCC, submitted-code execution, sandboxing, runtime injection,
broker/VM behavior, fd 3, private objects, and flags are intentionally absent.
Phase 2 must introduce compiler isolation without weakening the durable
outbox/at-least-once/idempotent database model.
