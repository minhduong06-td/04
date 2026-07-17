# Queue State Machine Design Review 03

Reviewed document: `docs/QUEUE_STATE_MACHINE_DESIGN.md`

Scope: design review only. No production code, migration, or Lua implementation
was written. The design document's current verdict was first downgraded to
`DESIGN NOT READY`; this review re-evaluates readiness after the eight external
blockers were incorporated into its authoritative sections. Review 02 remains
an unchanged historical READY decision for its narrower blocker set.

Historical note: Final Review 04 supersedes this readiness decision for lease
time, capability boundaries, DB dead-letter convergence, and outbox ordering.

## Review standard

Each blocker passes only if the design supplies all of:

1. authoritative Redis/PostgreSQL schema;
2. caller authorization and stable identity;
3. pre-mutation validation and ordered mutation;
4. explicit normal, conflict, replay, and outcome-unknown results;
5. concrete tests, including cross-store integration where applicable.

## 1. Recover versus ReleaseClaim — PASS

The shared `recovered_attempt` field was removed. A requeue now binds
`requeue_operation_id`, kind, attempt, generation, reason, and PostgreSQL event
sequence. Recover requires kind `recover`; ReleaseClaim requires kind `release`.

Replay is idempotent only when the complete operation binding matches. A second
operation for the same requeued attempt returns
`ALREADY_REQUEUED_BY_OTHER_OPERATION`; mismatched reuse of an operation ID
returns `OPERATION_CONFLICT`; a newer generation returns `SUPERSEDED`. The
losing outbox event is not marked delivered merely because another operation
requeued the attempt. The dispatcher must inspect and durably supersede it with
evidence of the winner.

Recover increments only recovery_count; Release increments only release_count.
Each applied operation records `attempt_outcome` and `attempt_operation`, so
outbox reconciliation can identify which operation actually won.

## 2. Claim retry after outcome unknown — PASS

The design adds read-only `InspectAttempt(attempt_id)`. Claim retry with the
same attempt ID uses the same lifecycle derivation. It distinguishes:

- active processing;
- requeued by Recover;
- requeued by ReleaseClaim;
- completed;
- failed;
- dead-letter;
- superseded by a newer generation;
- cleaned, using durable PostgreSQL attempt/cleanup history.

A valid transition after Claim is therefore not reported as corruption.
`CORRUPT_RECORD` is reserved for contradictory state/index/mapping evidence.
The attempt reverse maps and durable PostgreSQL queue_attempts rows provide the
retention boundary needed to explain outcomes after Redis Cleanup. Attempt IDs
come from a non-recycling PostgreSQL `(deployment_epoch, sequence)` allocator
and are reserved as `claim_reserved` rows before Redis Claim. Retention may
delete the row, but the allocator cannot reissue its opaque ID.

## 3. Lua integer range — PASS

All values parsed or incremented by Lua use canonical uint53, including
generation, attempt/recovery/release counts, queue/event sequences, time, and
lease deadlines. The maximum is 9007199254740991. Signed, padded, fractional,
exponent, non-finite, overflowing, and non-canonical forms are rejected before
mutation.

ClaimToken.Generation and DeliveryRecord fields now state uint53. Increment at
the maximum returns `NUMERIC_EXHAUSTED`; no operation relies on Lua number
precision beyond 2^53-1. The test plan covers every numeric field at max-1,
max, and overflow/rounded aliases.

## 4. Bounded Release/Claim retries — PASS

The design adds release_count and `MAX_RELEASE_ATTEMPTS=5` by default. A
distinct applied Release increments once; replay of the same operation does
not. Reaching the limit atomically dead-letters the delivery and records the
operation as the closing attempt operation.

Terminal replay is operation-sensitive: only the same operation that caused a
limit dead-letter returns `IDEMPOTENT_REQUEUE_DEAD_LETTER`. A terminal result
caused by another operation is superseded evidence, not idempotent success for
the requeue outbox.

Transient, outcome-known DB Start rejection may Release. Permanent DB identity
or state inconsistency may not cycle through Release; it creates PostgreSQL
audit plus Quarantine outbox. Every release has a PostgreSQL audit row, and the
limit outcome is recorded as `release_limit`. Together with the recovery limit,
this bounds successful requeues and pre-Cleanup attempt-map growth.

## 5. Partial Cleanup retry — PASS

Cleanup has a stable operation UUID, an immutable PostgreSQL attempt manifest,
and Redis `cleanup_op` plus `cleanup_phase` markers stored outside the delivery
fields being deleted. Different operation IDs conflict.

First cleanup validates terminal/retention/index/type/authorization conditions,
then writes the marker. Resume of the same operation accepts already absent
owned mappings and fields, rejects a remaining mapping owned by another
delivery, and continues ordered phases. Reverse attempt maps are removed before
the attempts SET; record fields follow; full absence is verified; marker removal
is last. A lost response after final removal is resolved using the durable
PostgreSQL cleanup row and full absence.

Unexpected Redis/runtime errors remain outcome-unknown; there is no assertion
that Lua rolls back prior writes. Fault-injection is required after every phase
and during reverse-map/record deletion.

## 6. Enqueue outbox rejection protocol — PASS

The PostgreSQL lifecycle now distinguishes `pending_enqueue`, `queued`,
`enqueue_rejected`, and `enqueue_unknown`.

- Redis applied/idempotent: DB becomes queued and Enqueue outbox is delivered.
- Definitive QUEUE_FULL: DB rejection, event superseded, durable source cleanup,
  HTTP 503.
- Definitive QUEUED_QUOTA: the same protocol with HTTP 409.
- Redis outcome unknown: DB becomes enqueue_unknown, source and undelivered
  outbox remain durable, and HTTP 202 truthfully reports enqueue_unknown.

The reconciler inspects the same delivery and retries the same Enqueue event.
It never deletes source or reports queued/rejected by inference. Source cleanup
is a durable PostgreSQL task created only after definitive rejection commits.
The protocol is compatible with an ordered PostgreSQL outbox and explicitly
requires migration from the current create-as-queued/direct-Enqueue flow.

## 7. RepairAttemptBinding — PASS

Option B was selected: Phase 1 has no RepairAttemptBinding operation. Missing or
corrupt active/reverse binding is ambiguous and always enters the PostgreSQL
audited Quarantine path. A processing record missing only ZSET membership may
be recovered when its remaining Redis token and exact DB token are unambiguous;
binding ambiguity itself is never repaired in place.

No undefined operator transition remains in the state machine or safe API.

## 8. Terminalization of a requeued delivery — PASS

Processing and queued terminalization are separate contracts:

- CompleteProcessing/FailProcessing require state=processing, exact active
  token, and the TerminalAuthorization committed with the PostgreSQL terminal
  row/outbox. They reject queued state.
- ApplyTerminalOutbox handles queued/requeued convergence. It additionally
  requires the exact requeue operation binding, terminal operation UUID,
  increasing event sequence, target outcome, and token.

The terminal operation/event binding is stored in Redis for replay. A delayed
worker using the ordinary processing method receives `STATE_CONFLICT`; it cannot
terminalize a queued delivery even with the old exact token. Only the trusted
dispatcher Go interface exposes the queued-terminal method; Review 04 clarifies
that Redis ACL is not a per-script boundary. A newer generation returns
`SUPERSEDED` without mutation.

## Eight-blocker test table

| Blocker | Required focused tests | Expected proof |
|---|---|---|
| Recover/Release identity | Recover then Release replay; Release then Recover replay; same ID with different kind/reason/token; same operation retry | only exact ID+kind binding is idempotent; losing event conflicts and counts do not change |
| Claim retry outcome | lose response then processing/recover/release/complete/fail/dead/new generation/cleanup; try to reuse cleaned attempt ID | exact lifecycle outcome, never corruption for a valid transition; non-recycling PG allocator prevents reuse |
| uint53 | canonical boundaries for generation/counts/sequences/time/deadline; 2^53 and rounded aliases | exact max-1 increment; explicit exhaustion/rejection before mutation |
| bounded Release | transient rejections through configured limit; replay each operation; permanent inconsistency | one increment per distinct operation; deterministic dead-letter/audit; immediate Quarantine for permanent cause |
| partial Cleanup | inject failure after marker and every deletion phase; same/different operation retry; foreign mapping | same ID resumes to full absence; different ID/foreign owner conflicts; no recreated or wrong deletion |
| Enqueue rejection | applied, QUEUE_FULL, QUEUED_QUOTA, timeout before/after Redis mutation, reconciliation restart, source cleanup crash | durable truthful DB/HTTP state, retained source on unknown, same-delivery convergence |
| binding corruption | missing active map, reverse mismatch, missing processing membership with otherwise exact token | ambiguous binding quarantines; only unambiguous missing membership recovers |
| queued terminalization | stale worker Complete/Fail; correct/wrong terminal op/event/requeue binding; replay; generation N+1 | worker cannot mutate queued; exact authorized event applies once; stale/conflict paths do not mutate |

## Cross-cutting regression review

- Claim still scans the complete bounded queue, so no prefix starvation was
  reintroduced.
- Queued/running quota still derives from authoritative queued/processing
  indexes; no lifecycle counter was restored.
- Processing remains a lease-deadline ZSET with deterministic expired scans and
  persistent structural-audit cursors.
- Delivery identity remains an opaque ZSET member; no payload identity,
  temporary marker, Redis list, or LREM was reintroduced.
- Recover and Release requeue at a new tail ready sequence; initial enqueue
  sequence remains immutable.
- Every expected validation/error branch precedes mutation. Unexpected runtime
  or transport failure is outcome-unknown and triggers inspect/retry, never
  compensating removal.
- Wrong key type is validated before mutation in every transition.
- PostgreSQL/Redis atomicity is not overstated. Enqueue, Claim/Start,
  requeue, terminal, quarantine, and cleanup windows all have durable outbox and
  reconciliation behavior.

## Current-code compatibility review

The design deliberately requires a future coordinated migration:

- `internal/queue/queue.go` currently exposes payload/list identity and
  tokenless Complete/Fail/RecoverOne; none satisfies this design.
- `internal/submissions/submissions.go` currently creates a queued DB row before
  Redis Enqueue and deletes source on Redis error; it must adopt the durable
  enqueue states/outbox protocol.
- `internal/database/database.go` lacks delivery/attempt/operation/outbox states
  and exact token CAS.
- `cmd/mock-worker/main.go` currently calls tokenless queue terminal/recovery
  methods and must consume ClaimToken plus PostgreSQL terminal authorization.

These are documented future implementation obligations, not changes made by
this review. API compatibility is compile-time/caller migration, not a promise
to preserve unsafe mutating signatures.

## Review checks performed

- Read the complete design, Review 02, AGENTS rules, clean queue code,
  submissions service, database layer, and mock-worker caller.
- Read the Phase 1 master/design/phase/security documents required by AGENTS.
- Searched the revised design for stale `recovered_attempt`, uint64 lifecycle
  fields, the old `ALREADY_RELEASED`, undefined RepairAttemptBinding behavior,
  bounded-prefix Claim, and worker terminalization of requeued state.
- Ran `git diff --no-index --check /dev/null <file>` for each new design/review
  document; exit 1 is the expected no-index difference status and all three
  emitted zero whitespace-error diagnostics. No production tests were run
  because this task changes documentation only and makes no implementation claim.

## Verdict

**DESIGN READY**

All eight blockers now have schema, authorization, validation/mutation order,
conflict and outcome-unknown behavior, PostgreSQL reconciliation, and explicit
tests. This verdict approves the design for a later implementation review; it
does not accept the current production queue as conforming.
