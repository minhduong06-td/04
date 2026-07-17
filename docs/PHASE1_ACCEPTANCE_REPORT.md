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
make phase1-acceptance                                                 PASS
gofmt -l .                                                             PASS (no output)
GOCACHE=/tmp/hustack-go-cache go mod tidy                              PASS
git diff -- go.mod go.sum                                              PASS (no changes)
GOCACHE=/tmp/hustack-go-cache go vet ./...                             PASS
GOCACHE=/tmp/hustack-go-cache go build ./...                           PASS
GOCACHE=/tmp/hustack-go-cache go test ./...                            PASS
GOCACHE=/tmp/hustack-go-cache go test -race ./...                      PASS
docker compose config --quiet                                         PASS
git diff --check                                                       PASS
```

The clean gate classifies test coverage instead of mixing optional developer
runs with required dependency tests:

- 38 dependency-free internal test cases, run normally and with the race detector;
- 3 required real PostgreSQL cases, plus their race run;
- 6 required real Redis 7 queue cases, plus their race run;
- 6 required real Redis 7 rate-limit cases, plus their race run;
- 24 normal HTTP integration cases; and
- 4 isolated compliance cases.

That is 134 classified case executions in the clean gate. Required skipped-test
count: **0**. The closure narrow gate additionally ran all six rate-limit cases
10 times (60 executions) and under the race detector 5 times (30 executions),
also with zero skips.

Required Redis rate-limit commands in the clean gate are:

```text
REDIS_ADDR=127.0.0.1:26379 HUSTACK_REQUIRE_REDIS=1 go test -v ./internal/ratelimit -count=1       PASS (6/6)
REDIS_ADDR=127.0.0.1:26379 HUSTACK_REQUIRE_REDIS=1 go test -race ./internal/ratelimit -count=1    PASS (6/6)
```

The six executed cases are `TestParticipantSubmitRateLimit`,
`TestIPSubmitRateLimit`, `TestTokenRefill`,
`TestDifferentParticipantsDontAffect`, `TestNoRaceOnRefresh`, and
`TestPollRateLimit`.

The isolated cases prove all previously missing acceptance evidence:

- direct loopback API requests allow two submissions and return an application
  JSON 429 with a positive `Retry-After` on the third;
- a test-only Nginx limit of 1 request/minute produces a distinct non-JSON 429;
- `QUEUE_MAX_DEPTH=1` accepts one participant and returns 503 with
  `Retry-After` for a second participant while PostgreSQL remains at exactly
  one submission/one outbox row and the source volume gains only the accepted
  file;
- a controlled PostgreSQL fixture stores literal hostile stdout/stderr, the
  owner JSON receives the exact strings as data, server-rendered HTML does not
  embed them, and a focused source check enforces text-only frontend sinks.

The application-rate case also found and fixed a malformed `Retry-After`
conversion that appended a NUL byte; `internal/web` now has a regression test.

## Fresh image and service evidence

The acceptance sequence ran `docker compose down -v --remove-orphans`, then
`docker compose build --no-cache api mock-worker`, followed by force recreation.
Fresh image IDs reported were:

- API: `sha256:4b0450b941578a2db624f5feeeecac5dd5bccac236847b3c224eea7293565a01`
- mock-worker: `sha256:c7bdfa988bc8eb1e2b0b34238bb22e1f6398682821c29ea58785169d801d2b1c`

PostgreSQL, Redis, API, and Nginx reported healthy; mock-worker remained running.
Only Nginx publishes a host port. Readiness checks PostgreSQL, Redis, and an
actual create/write/remove operation in source storage; healthz remains
liveness-only.

Integration coverage verified textarea and `.c` submissions, immediate queued
responses, asynchronous completion, owner-only access, actual hostile result
handling, size limits including 10 MiB boundaries, CSRF/origin handling,
application and Nginx 429 responses, global-capacity 503 atomicity, and health
routes. Database concurrency tests verify participant/global quotas and one
running job per participant. Minute/hour/IP/poll token buckets are now required
real-Redis tests rather than optional developer-only coverage.

`make phase1-acceptance` is the reproducible destructive gate. It starts with a
volume-clean normal stack and no-cache API/worker build, runs normal tests, then
uses a separate Compose project with only loopback test ports for PostgreSQL,
Redis, and the direct API. It removes that project and its volumes and restores
the normal healthy stack before returning. Normal Compose still publishes only
Nginx on port 8080.

README and the security notes now document PostgreSQL lifecycle/quota authority,
the transactional outbox, Redis Streams delivery/XAUTOCLAIM, duplicate-delivery
safety, stale database recovery, and the non-exactly-once XACK-then-XDEL caveat.

## Phase 2 limitations

Compilation, GCC, submitted-code execution, sandboxing, runtime injection,
broker/VM behavior, fd 3, private objects, and flags are intentionally absent.
Phase 2 must introduce compiler isolation without weakening the durable
outbox/at-least-once/idempotent database model.
