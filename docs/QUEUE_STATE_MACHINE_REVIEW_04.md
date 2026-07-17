# Queue State Machine Final Design Review 04

Reviewed document: `docs/QUEUE_STATE_MACHINE_DESIGN.md`

Scope: documentation-only final correction for exactly four external blockers.
No production code, migration, Lua implementation, OpenCode task, commit, or
push was performed.

## Review standard

Each blocker passes only when the design contains:

1. authoritative schema or startup-validated configuration;
2. an exact selection/transition algorithm and authorization boundary;
3. definitive, conflicting, response-lost, and outcome-unknown behavior;
4. idempotent retry rules;
5. real Redis/PostgreSQL/deployment integration tests.

## 1. Lease time authority and timeout policy — PASS

Redis server TIME is now the only lease clock. Claim, InspectAttempt,
RecoveryCandidates, and Recover take no caller-provided `now`. Claim and Recover
call TIME exactly once inside their Lua invocation after non-time validation and
before mutation. RecoveryCandidates and InspectAttempt use read-only scripts
with one TIME call.

The TIME pair is parsed as canonical non-negative seconds and microseconds in
`[0,999999]`, then converted to uint53 milliseconds with an explicit
pre-multiplication bound. Malformed/overflowing replies return
`REDIS_TIME_INVALID` before mutation. Claim validates Redis milliseconds plus
lease duration before storing the processing ZSET deadline. Recover uses
`deadline <= redis_now_ms`; it rechecks after candidate/network delay.

Network delay only consumes lease time. Before DB Start, InspectAttempt returns
Redis-derived remaining time. Work starts only with at least hard timeout plus
safety margin remaining. The hard timeout begins on receipt and includes DB
Start, mock work, and terminal DB/outbox commit. Timeout cancellation commits an
exact-token internal_error event when possible; otherwise it leaves Redis for
lease recovery without a compensating mutation.

Configuration now specifies and validates:

- `MOCK_WORK_HARD_TIMEOUT=5s`;
- `QUEUE_LEASE_SAFETY_MARGIN=2s`;
- `QUEUE_LEASE_DURATION=10s`;
- `REDIS_OPERATION_TIMEOUT=500ms`;
- mock duration <= hard timeout;
- lease duration >= hard timeout + safety margin;
- Redis timeout < safety margin;
- positive, whole-millisecond, canonical uint53 representations.

No RenewLease is needed for Phase 1 under these invariants. Phase 2 must review
renewal before changing runtime duration.

## 2. Enforceable worker/dispatcher capability boundary — PASS

Phase 1 explicitly chooses trusted process boundary option A. The design no
longer claims Redis ACL can authorize one EVAL/EVALSHA script but deny another.
Redis ACL authenticates trusted services, limits key prefixes/commands, and
denies administrative commands; it is defense in depth, not per-script or
worker-versus-dispatcher authorization.

The Go interfaces are split into APIQueue, WorkerQueue, and DispatcherQueue.
Raw Redis clients/scripts stay private. Mock-worker receives processing-only
methods; the separate trusted dispatcher receives recovery, release, queued
terminalization, quarantine, and cleanup methods. This is an enforceable
application/deployment boundary against submitted/untrusted processes, not a
claim that a compromised trusted worker is untrusted.

Deployment separates front, trusted-data, and untrusted networks. Only trusted
Go services receive Redis address/auth material and trusted-data routing. Redis
is not published or attached to untrusted. Submitted Phase 1 source remains
inert and receives no credential, environment, socket, inherited connection,
file descriptor, volume, or route.

Acceptance tests render deployment configuration without credential values and
run an ephemeral untrusted-network probe that cannot resolve/connect Redis and
has no Redis material/descriptors. The current single-network, unauthenticated
Compose file is correctly identified as an implementation gap rather than proof
of the new boundary.

## 3. Recovery-limit and Quarantine PostgreSQL convergence — PASS

Recovery R1 still uses exact-token `mock_processing -> queued` plus one Recover
audit/outbox operation. The missing convergence is now explicit:

- normal recovered/idempotent result delivers the event and leaves DB queued;
- recovery-limit applied or same-operation idempotent dead-letter continues the
  ordered-dispatch DB transaction with exact operation/token checks;
- it CASes queued -> internal_error, stores reason `recovery_limit` and terminal
  operation ID, updates attempt/audit evidence, and delivers that exact event;
- replay accepts only the same internal_error operation/reason/token;
- response loss requires InspectAttempt/InspectDelivery to prove dead state and
  exact `attempt_operation` before applying the DB transaction;
- a dead-letter created by another operation never delivers the current event.

Quarantine similarly binds one PostgreSQL audit/outbox operation to Redis
`quarantine_op`. Exact applied/idempotent results converge allowed nonterminal
DB states to internal_error with `quarantine:<reason>`, preserve audit evidence,
and deliver only the matching event. Lost responses require dead membership plus
the exact quarantine operation. Conflicting operation/token evidence remains
pending for reconciliation.

This removes the invalid steady state where Redis is dead-letter while
PostgreSQL remains queued after a recovery-limit result.

## 4. Actual ordered outbox selection algorithm — PASS

The dispatcher no longer treats advisory locking as ordering by itself. Its
authoritative transaction is:

```text
BEGIN
pg_advisory_xact_lock(delivery_id)
SELECT the lowest unresolved event
  WHERE delivered_at IS NULL AND superseded_at IS NULL
  ORDER BY event_seq ASC LIMIT 1 FOR UPDATE
dispatch only that row
mark delivered, explicitly superseded, or update retry evidence
COMMIT
```

Candidate delivery selection before this transaction is only a hint. Two
dispatchers for one delivery serialize before the post-lock ordered row query;
different deliveries can progress concurrently. N+1 is never dispatched while
N remains unresolved or is waiting for backoff.

Crash before Redis leaves N pending. Crash after Redis but before DB
acknowledgement retries/inspects the same stable operation. Retry metadata uses
capped exponential backoff. Another winning operation requires exact Redis plus
durable DB evidence and an audited `superseded_by_operation_id`; it is not marked
delivered as N.

After retry-count/age thresholds, a poison head enters an explicit decision
state and raises an alert. Only an idempotent audit decision proving applied,
proving a winner, or authorizing Quarantine may deliver/supersede it. Retry
exhaustion alone cannot skip it, and N+1 remains visibly blocked until that
decision commits.

## Four-blocker test table

| Blocker | Required integration/concurrency tests | Required proof |
|---|---|---|
| Lease authority | fast/slow worker clocks; Redis TIME parse/bounds; deadline boundary; candidate delay; hard timeout before expiry | worker clocks cannot alter lease/staleness; invalid time/duration makes no mutation |
| Capability boundary | rendered network/secret ownership; untrusted connection/DNS/env/fd probe; Go interface compile boundary; Redis admin-command denial | no submitted/untrusted path to Redis; no false per-script ACL claim |
| DB convergence | recovery-limit applied/replay/response loss/other winner; Quarantine applied/replay/response loss/conflict | Redis dead converges to exact-operation DB internal_error and only matching outbox is delivered |
| Ordered outbox | concurrent dispatchers N/N+1; crash before/after Redis; backoff/restart; other winner; poison decision | lowest unresolved event only; retries are idempotent; unblock requires committed deliver/audited supersede |

## Current implementation gaps confirmed

- `internal/config/config.go` has MockWorkDuration but no hard timeout, lease,
  safety margin, Redis operation timeout, outbox retry policy, or relationship
  validation.
- `internal/queue/queue.go` uses `time.Now()` for Claim/RecoverStale and exposes
  tokenless list/payload transitions.
- `internal/database/database.go` has no queue outbox/audit operation schema or
  exact-token convergence transactions.
- `cmd/mock-worker/main.go` owns a raw queue pointer and calls tokenless recovery
  and terminal methods.
- `docker-compose.yml` uses one internal network, unauthenticated Redis, and no
  dedicated dispatcher.

These are future implementation obligations. This final review does not claim
the current production code conforms.

## Verification performed

- Read the complete design, Review 03, AGENTS rules, config, queue,
  submissions, database, mock-worker, and Compose files.
- Searched for caller-time lease APIs, advisory-lock-only ordering, and Redis
  per-script ACL claims.
- `git diff --check` exited 0 with no output. Because these design documents are
  untracked, each revised/new document was also checked with
  `git diff --no-index --check /dev/null <file>`; exit 1 was the expected content
  difference status and all produced zero whitespace diagnostics.
- No production tests were run because only documentation was changed.

## Verdict

**DESIGN READY — FROZEN**

All four final blockers have schema/configuration, exact algorithms, failure
outcomes, idempotent retry, and integration tests. No further design review is
opened.

Proposed implementation checkpoint A:

```text
Redis v3 schema
+ read-only InspectDelivery/InspectAttempt
+ Enqueue only
```

Claim and every other lifecycle mutation remain outside checkpoint A.
