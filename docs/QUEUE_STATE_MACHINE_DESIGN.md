# Queue State Machine Design — Phase 1

Status: design-only. This document does not authorize implementation.

## 1. Scope and non-negotiable rules

This design replaces the legacy payload/list queue. It does not use the Cycle
05 implementation as a base; Cycle 04/05 reviews are failure-mode inputs only.
PostgreSQL remains authoritative for the user-visible submission and result.
Redis is authoritative for current queue membership, attempt ownership, leases,
and ordering.

The requested `../queue-cycle05-backup/agent_handoff` directory was absent when
the original design was prepared. Equivalent workspace reviews under
`docs/agent_handoff` and clean `HEAD:internal/queue/queue.go` were used to map
legacy APIs. This does not weaken any requirement below.

Rules:

- A queue member is an opaque `delivery_id`, never a serialized payload.
- Every Claim has a stable `attempt_id` and monotonically increasing generation.
- No temporary marker, Redis list, or `LREM` is used.
- Enqueue, Claim, Complete, Fail, Recover, ReleaseClaim, Quarantine, and Cleanup
  are separate atomic Lua transitions.
- All expected argument, type, state, index, quota, token, and lease validation
  happens before the first mutation.
- An expected validation result never follows a mutation.
- A transport timeout or unexpected Redis script/runtime error is
  outcome-unknown, even though scripts are atomic to other Redis clients.
- Callers never compensate by deleting/moving a member. They inspect and retry
  by delivery ID and token, or let reconciliation converge it.
- Critical state has no TTL and the Redis namespace uses no-eviction policy.
- Redis server time, obtained inside the relevant script, is the only lease
  clock. Worker wall clocks never create deadlines or decide staleness.
- Phase 1 retains `QUEUE_MAX_DEPTH=500`; increasing it requires a new
  performance review because quota and fairness deliberately use bounded O(N)
  scans.

## 2. Identity, attempts, and queue ordering

### 2.1 Delivery identity

`delivery_id` is generated once for one logical job. The existing opaque
submission UUID is globally unique, so Phase 1 uses:

```text
delivery_id = submission_id
```

This is direct identity equality, not an encoding. Participant and submission
metadata remain separate fields. No code splits a queue member to reconstruct
metadata.

### 2.2 Attempt identity and claim token

The worker obtains an opaque `attempt_id` from a PostgreSQL allocator before
Claim and inserts a `claim_reserved` row. The ID is the fixed canonical encoding
of `(deployment_epoch, non-recycling database sequence)`; Lua treats it as an
opaque string and never parses the sequence. The allocator never accepts a
caller-supplied ID, never resets/reuses its sequence, and returns an explicit
exhaustion error. Thus retention can delete old attempt rows without making an
old ID allocatable again. Redis worker access accepts only an active reservation.
The worker reuses that reservation if Claim outcome is unknown. Claim increments
the selected delivery generation and returns:

```text
ClaimToken {
    DeliveryID string
    AttemptID  string       # canonical allocator ID, not serialized payload
    Generation uint53
}
```

`uint53` means a canonical base-10 integer in `[0, 9007199254740991]`. Lua may
parse or increment no wider integer. Claim refuses generation or attempt-count
exhaustion before mutation.

Processing Complete/Fail, Recover, and ReleaseClaim require the complete token.
Token matching always compares all three fields. Attempt mappings persist
through the audit/retention window and are removed only by coordinated Cleanup.

### 2.3 Operation identity

An attempt identifies a Claim, not the operation that later requeues or
terminalizes it. Every durable outbox transition therefore has its own UUID:

```text
RequeueOperation {
    OperationID string       # UUID, stable across dispatch retry
    Kind        recover|release
    Attempt     ClaimToken
    Reason      closed enum
    EventSeq    uint53       # PostgreSQL per-delivery event sequence
}

TerminalAuthorization {
    OperationID      string
    EventSeq         uint53
    Target           completed|failed|dead-letter
    Attempt          ClaimToken
    RequeueOperationID string|null
    RequeueKind      recover|release|null
}
```

Idempotency requires equality of operation ID, kind, token, reason, and event
sequence. Equality of attempt alone is never enough. A different operation on
an already-requeued attempt returns an explicit conflict/superseded result and
cannot be marked delivered as if it were the original operation.

### 2.4 Initial FIFO and requeue position

Two sequences are distinct:

- `enqueue_seq`: immutable first-enqueue order, retained for audit.
- `ready_seq`: the current position in the queued ZSET.

Initial Enqueue allocates one sequence and sets both fields to it. Claim examines
the entire queued ZSET in ascending `ready_seq`; therefore the oldest eligible
delivery is selected and quota-blocked participants cannot hide an eligible job
outside a prefix.

Recover and ReleaseClaim allocate a new `ready_seq` and return the delivery to
the tail. FIFO is strict for first enqueue and for the current ready epoch, not
an unconditional promise that a crashing delivery keeps the head forever.

## 3. Phase 1 performance assumptions

`QUEUE_MAX_DEPTH=500` is interpreted as a bound on all live queue work:

```text
ZCARD(queued) + ZCARD(processing) <= 500
```

Enqueue does one full queued scan to count queued work for its participant.
Claim reads all processing members once to build an in-Lua
`running_by_participant` map, then reads all queued members once in ready order.
The cost is O(P+Q), with P+Q <= 500, rather than O(P*Q).

This bounded full scan is a conscious Phase 1 simplicity/security tradeoff. The
Lua duration is measured and alerted. Configuration validation rejects
`QUEUE_MAX_DEPTH > 500` unless a future design adds authoritative participant
membership indexes and repeats the security review.

There are no authoritative queued/running counters. Queue depth, participant
quota, and lifecycle correctness come from indexes and record metadata.
Telemetry may cache counts outside the transition namespace, but scripts and
callers never use that cache for decisions; it may be dropped and rebuilt from
the authoritative indexes at any time.

### 3.1 Lease clock and Phase 1 timeout policy

Phase 1 adds four startup-validated duration settings:

```text
MOCK_WORK_HARD_TIMEOUT=5s
QUEUE_LEASE_SAFETY_MARGIN=2s
QUEUE_LEASE_DURATION=10s
REDIS_OPERATION_TIMEOUT=500ms
```

All are positive whole milliseconds whose millisecond representation is a
canonical uint53. `MOCK_WORK_DURATION <= MOCK_WORK_HARD_TIMEOUT` and:

```text
QUEUE_LEASE_DURATION >= MOCK_WORK_HARD_TIMEOUT + QUEUE_LEASE_SAFETY_MARGIN
```

must hold without integer overflow. `REDIS_OPERATION_TIMEOUT` is positive and
strictly less than `QUEUE_LEASE_SAFETY_MARGIN`. API, worker, and dispatcher fail
startup on violation; they do not silently use a fallback. Phase 1 does not
implement RenewLease under these invariants.

Claim and every stale decision use Redis server time. Inside one Lua invocation,
after all type/state/token validations that do not depend on time and before the
first mutation, the script calls `redis.call("TIME")` exactly once. Redis 7 TIME
must return two canonical decimal strings `[seconds, microseconds]`.
`microseconds` must be in `[0,999999]`; `seconds` must be non-negative and no
larger than:

```text
floor((UINT53_MAX - floor(microseconds/1000)) / 1000)
```

The script then computes:

```text
redis_now_ms = seconds * 1000 + floor(microseconds / 1000)
```

All operands/results are checked before arithmetic that could lose precision.
A malformed/out-of-range TIME reply returns `REDIS_TIME_INVALID` before any
mutation. An unexpected TIME command/runtime failure is also pre-mutation for
Claim/Recover and is returned as an operation error, not guessed from a worker
clock.

Claim validates `redis_now_ms + lease_duration_ms <= UINT53_MAX` and stores that
sum as the processing ZSET score. Recover obtains its own TIME value in the same
Recover invocation and treats a valid lease as stale exactly when
`lease_deadline_ms <= redis_now_ms`. RecoveryCandidates is a read-only Lua script
that calls TIME once and performs `ZRANGEBYSCORE processing -inf redis_now_ms`;
Recover always rechecks, so candidate transport delay is harmless.

Network delay after Claim reduces, never extends, the usable lease. Before DB
Start, the worker calls read-only InspectAttempt, whose script uses TIME and
returns server-derived
`lease_remaining_ms=max(lease_deadline_ms-redis_now_ms,0)`. It starts work
only when the exact attempt is processing and remaining time is at least
`MOCK_WORK_HARD_TIMEOUT + QUEUE_LEASE_SAFETY_MARGIN`; otherwise it creates
ReleaseClaim and performs no job work. The hard-timeout context begins as soon
as that Inspect response is received and includes DB Start, mock work, and the
terminal DB/outbox commit. The bounded Redis response delay consumes part of the
safety margin, never the hard execution window; an excessively delayed/timed-out
response causes no Start and is inspected/retried.

Once DB Start commits, the worker runs all Phase 1 work and terminal DB/outbox
commit under a cancellable hard-timeout context. At timeout it stops mock work,
commits an exact-token `internal_error` terminal event with reason
`worker_timeout` when possible, and dispatches/retries normally. If DB is
unavailable or shutdown cancellation wins, it performs no blind Redis mutation;
the lease expires and recovery converges the attempt. Tests must prove the hard
timeout fires before lease expiry using Redis-derived remaining time.

Phase 2 changes execution duration and isolation. It must review whether lease
renewal is required before changing any timeout or lease setting; it may not
inherit Phase 1's no-renewal decision automatically.

## 4. Redis key schema

Every key uses one Cluster hash tag, for example `hq:{phase1}:v3`. Scripts
receive fixed keys in `KEYS`; ARGV cannot select an arbitrary Redis key.

| Suffix | Type | Authoritative contents |
|---|---|---|
| `:schema` | string | exact version `3` |
| `:next_seq` | string canonical uint53 | last allocated ready/enqueue sequence |
| `:queued` | sorted set | member delivery ID, score current `ready_seq` |
| `:processing` | sorted set | member delivery ID, score Redis-TIME-derived lease deadline ms |
| `:dead` | sorted set | member delivery ID, score original `enqueue_seq` |
| `:state` | hash | delivery ID -> state |
| `:submission` | hash | delivery ID -> immutable submission ID |
| `:participant` | hash | delivery ID -> immutable participant ID |
| `:enqueue_seq` | hash | delivery ID -> original sequence |
| `:ready_seq` | hash | delivery ID -> current queued sequence; absent otherwise |
| `:generation` | hash | delivery ID -> canonical uint53 current generation |
| `:attempt_count` | hash | delivery ID -> canonical uint53 successful Claims |
| `:recovery_count` | hash | delivery ID -> canonical uint53 applied Recover operations |
| `:release_count` | hash | delivery ID -> canonical uint53 applied ReleaseClaim operations |
| `:active_attempt` | hash | delivery ID -> processing attempt ID |
| `:requeue_op_id` | hash | delivery ID -> last applied requeue operation UUID |
| `:requeue_op_kind` | hash | delivery ID -> `recover` or `release` |
| `:requeue_attempt` | hash | delivery ID -> attempt requeued by that operation |
| `:requeue_generation` | hash | delivery ID -> canonical uint53 generation requeued |
| `:requeue_reason` | hash | delivery ID -> closed reason code |
| `:requeue_event_seq` | hash | delivery ID -> canonical uint53 PG event sequence |
| `:terminal_attempt` | hash | delivery ID -> terminalizing attempt ID |
| `:terminal_op_id` | hash | delivery ID -> authorized terminal outbox UUID, if used |
| `:terminal_event_seq` | hash | delivery ID -> canonical uint53 terminal event sequence |
| `:lease_attempt` | hash | delivery ID -> attempt ID owning processing score |
| `:attempt_delivery` | hash | attempt ID -> delivery ID |
| `:attempt_generation` | hash | attempt ID -> canonical uint53 generation |
| `:attempt_outcome` | hash | attempt ID -> processing/recovered/released/completed/failed/dead/superseded |
| `:attempt_operation` | hash | attempt ID -> requeue/terminal operation UUID that closed it |
| `:quarantine_op` | hash | delivery ID -> PostgreSQL audit/outbox operation ID |
| `:cleanup_op` | hash | delivery ID -> in-progress cleanup authorization UUID |
| `:cleanup_phase` | hash | delivery ID -> started/reverse_maps/attempt_set/record/verified |
| `:attempts:<delivery_id>` | set | all attempt IDs allocated for one delivery |

The per-delivery attempts key is constructed from a validated UUID, shares the
same hash tag, is passed in `KEYS`, and its exact expected name is checked by the
wrapper and script. It exists so Cleanup can remove every reverse attempt map.

There is deliberately no `queued_count`, `running_count`, or lease deadline
HASH. The processing ZSET is both processing membership and authoritative lease
deadline. Redis guarantees ZSET scores are numeric; scripts reject non-finite,
negative, overflow, or otherwise out-of-policy scores before mutation.

All fixed key types are validated at script start using a Redis-7-safe TYPE
normalizer. `none` is allowed only where namespace initialization or a new
per-delivery attempts set permits it. Wrong type always returns before mutation.

All numeric values read or changed by Lua--sequences, generations, counts,
event sequences, deadlines, and current time--must be canonical uint53. ZSET
scores are exact only in this range. No script uses a Go `uint64` contract and
then calls Lua `tonumber`. Exhaustion returns a specific no-mutation result.
For a live delivery that can no longer progress, PostgreSQL records an audited
Quarantine outbox operation; the failing transition itself does not mutate.

## 5. Logical delivery record and state invariants

```text
DeliveryRecord {
    delivery_id:       UUID, immutable
    submission_id:     UUID, immutable
    participant_id:    UUID, immutable
    state:             absent|queued|processing|completed|failed|dead-letter
    enqueue_seq:       uint53, immutable
    ready_seq:         uint53 when queued
    generation:        uint53, monotonic
    attempt_count:     uint53, monotonic
    recovery_count:    uint53, monotonic
    release_count:     uint53, monotonic
    active_attempt:    AttemptID when processing
    requeue_operation_id: UUID after a requeue
    requeue_operation_kind: recover|release after a requeue
    requeue_attempt_id: AttemptID after a requeue
    requeue_generation: uint53 after a requeue
    requeue_reason:     closed enum after a requeue
    requeue_event_seq:  uint53 after a requeue
    terminal_attempt:  AttemptID for terminal state
    terminal_operation_id: UUID for outbox-authorized terminalization
    terminal_event_seq: uint53 for outbox-authorized terminalization
    lease_attempt:     AttemptID when processing
}
```

| State | Queued ZSET | Processing ZSET | Dead ZSET | Token fields |
|---|---:|---:|---:|---|
| absent | no | no | no | no record fields |
| queued, initial | exactly once at `ready_seq` | no | no | no active/requeue fields |
| queued, requeued | exactly once at `ready_seq` | no | no | complete requeue operation binding retained |
| processing | no | exactly once at lease deadline | no | active and lease attempt required |
| completed | no | no | no | terminal attempt and terminal authorization binding required as applicable |
| failed | no | no | no | terminal attempt and terminal authorization binding required as applicable |
| dead-letter | no | no | exactly once | terminal or quarantine operation required |

The same delivery can never validly appear in more than one live/dead index.

## 6. State transition table

| From | Operation | To | Authoritative index effect | Replay/conflict |
|---|---|---|---|---|
| absent | Enqueue valid immutable record | queued | add queued at new sequence | applied |
| existing valid | Enqueue same immutable record | unchanged | none | idempotent existing |
| existing corrupt | Enqueue | unchanged | none | corrupt record |
| queued | Claim exact eligible item | processing | add processing, remove queued | token returned |
| processing | CompleteProcessing exact token plus terminal authorization | completed | remove processing | applied |
| processing | FailProcessing exact token plus terminal authorization | failed/dead-letter | remove processing; optionally add dead | applied |
| queued/requeued | ApplyTerminalOutbox exact PG authorization | terminal | remove queued; optionally add dead | authorized convergence |
| processing | Recover exact operation/token below limit | queued tail | add queued, remove processing | same operation idempotent |
| processing | Recover eligible lease at recovery limit | dead-letter | add dead, remove processing | poison policy |
| processing | ReleaseClaim exact operation/token below limit | queued tail | add queued, remove processing | no lease-age guard |
| requeued | same requeue operation ID/kind/token | unchanged | none | idempotent requeue |
| requeued | different operation for same attempt | unchanged | none | operation conflict/already requeued by other |
| any newer generation | old terminal/recover/release token | unchanged | none | stale attempt |
| terminal | terminal replay | unchanged | none | existing terminal result |
| corrupt queued/processing | Quarantine authorized operation | dead-letter | exact source removal, add dead | audited/idempotent |
| eligible terminal | Cleanup authorized operation | absent | delete record/attempt fields | idempotent clean |

Completed never becomes failed; failed never becomes completed. ReleaseClaim is
not a substitute for lease recovery: it increments `release_count`, never
`recovery_count`. Ambiguous attempt-binding corruption has no Phase 1 repair
operation; it is quarantined through the audited PostgreSQL outbox path.

Redis `dead-letter` caused by a recovery limit or Quarantine must converge to
PostgreSQL `internal_error` with the exact closing operation ID, attempt token,
and reason. Redis dead plus a PostgreSQL queued/nonterminal row is a temporary
outbox window, never a completed reconciliation state.

## 7. Common Lua contract and outcome-unknown rule

Each transition is structured as:

```text
READ/VALIDATE PHASE
  validate ARGV syntax/ranges and closed enums
  normalize and validate every Redis key TYPE
  validate schema
  read all record, index, token, quota, lease, and operation fields needed
  parse canonical integers (no sign, spaces, fraction, exponent, NaN, overflow)
  decide every expected error/no-op result

MUTATION PHASE
  use only commands whose types and arguments were validated
  add the new recoverable index before removing the old index
  update record fields
  return APPLIED/IDEMPOTENT only; no expected error branch follows a write
```

Lua serialization is not rollback. A Redis command can still fail unexpectedly
after an earlier write because of server/runtime faults. Therefore both a
transport timeout and any unexpected script/runtime failure are
**outcome-unknown**. The caller must not issue compensating ZREM/ZADD/HDEL calls.
It retries the same operation ID/token where idempotent, calls a read-only
`InspectDelivery(delivery_id)`, and lets the reconciler compare actual state,
indexes, PostgreSQL token, and outbox event.

Redis OOM write refusal, process loss, AOF/fsync policy, and storage rollback are
operational failure domains. No-eviction, persistence monitoring, outcome
inspection, and PostgreSQL reconciliation are acceptance requirements.

## 8. Atomic transition pseudocode

### 8.1 Enqueue

Inputs: immutable Delivery, `max_active=500`, participant queued limit.

Validation:

```text
validate types, schema, IDs, limits, sequence
if state[delivery_id] exists:
    validate submission, participant, sequences, generation, attempt/recovery/
      release counts, operation fields, and exact state-specific membership across queued,
      processing, and dead indexes
    if any invariant is missing/inconsistent: return CORRUPT_RECORD
    if immutable metadata differs: return DELIVERY_CONFLICT
    return IDEMPOTENT_EXISTING
require no record fields or index membership for delivery_id
require ZCARD(queued)+ZCARD(processing) < max_active
queued_members = ZRANGE queued 0 -1
for each member:
    validate member has state=queued and participant metadata
    count members whose participant equals new participant
require participant queued count < max_queued
require next_seq valid and not exhausted
```

Mutation order:

```text
seq = next_seq + 1
SET next_seq seq
HSET immutable fields, state=queued, enqueue_seq=seq, ready_seq=seq,
     generation=0, attempt_count=0, recovery_count=0, release_count=0
ZADD queued seq delivery_id
return APPLIED
```

Idempotent success is impossible for a state=queued record missing its queued
membership, a processing record missing its processing score/token, a terminal
record missing terminal metadata, or any cross-index duplicate.

### 8.2 Claim — complete bounded scan, no starvation

Inputs: caller `attempt_id`; startup-validated queue policy supplies lease
duration and max running. There is no caller `now` or per-call duration.

Validation/selection:

```text
validate types, schema, canonical reserved attempt ID and lease duration
if attempt_delivery[attempt_id] exists:
    validate reverse ownership and attempt_generation, then derive the actual
      lifecycle from attempt_outcome plus the current delivery record
    return CLAIMED_PROCESSING, REQUEUED_BY_RECOVER,
      REQUEUED_BY_RELEASE, COMPLETED, FAILED, DEAD_LETTER, or SUPERSEDED
    return CORRUPT_RECORD only for contradictory metadata, not for a valid
      transition that happened after the original Claim

processing_members = ZRANGE processing 0 -1 WITHSCORES
running_by_participant = empty map
for every processing member:
    validate state=processing, no queued/dead membership, participant metadata,
      active/lease attempt binding, generation mapping, finite policy-valid score
    running_by_participant[participant]++

queued_members = ZRANGE queued 0 -1 WITHSCORES   # full queue, maximum 500
if empty: return NO_JOB
for every queued member in ready_seq order:
    validate state=queued, exact ready score/metadata, no processing/dead
      membership, no active/lease attempt, complete immutable fields
    if running_by_participant[participant] >= max_running:
        mark quota-blocked and continue
    select the first eligible member and stop
if none selected: return QUOTA_BLOCKED
validate selected generation/attempt_count can increment within uint53
TIME exactly once; validate/convert reply to redis_now_ms as section 3.1
validate deadline=redis_now_ms+lease_duration without uint53 overflow
```

Because the script scans all queued members, an eligible delivery cannot remain
unseen behind any number of quota-blocked entries. The statement “one
participant cannot block the global queue” is thus proven only under the hard
active bound of 500.

Mutation order:

```text
generation++
attempt_count++
ZADD processing deadline delivery_id       # preserve before removal
HSET active_attempt delivery_id attempt_id
HSET lease_attempt delivery_id attempt_id
HSET generation and attempt_count
HSET attempt_delivery attempt_id delivery_id
HSET attempt_generation attempt_id generation
HSET attempt_outcome attempt_id processing
SADD attempts:<delivery_id> attempt_id
if validated queued record has requeue_attempt:
    HSET attempt_outcome requeue_attempt superseded
HSET state processing
HDEL ready_seq and all requeue operation fields for delivery_id
ZREM queued delivery_id
return APPLIED_CLAIM(token and immutable job metadata)
```

Running quota is derived from the processing index in the same script. Two
concurrent Claims serialize and the second sees the first processing member.

### 8.3 InspectAttempt and Claim outcome-unknown retry

`InspectAttempt(attempt_id)` is read-only. It validates fixed key types, reverse
ownership, generation, attempts-set membership, and the state-specific record.
For processing state it calls TIME exactly once and returns Redis-derived
deadline and remaining milliseconds; caller time is ignored.
It returns:

```text
CLAIMED_PROCESSING(token, lease_deadline)
REQUEUED_BY_RECOVER(token, operation_id, reason, ready_seq)
REQUEUED_BY_RELEASE(token, operation_id, reason, ready_seq)
COMPLETED(token, terminal_operation_id?)
FAILED(token, terminal_operation_id?)
DEAD_LETTER(token, closing_operation_id)
SUPERSEDED(token, current_generation)
CLEANED(operation_id)       # from durable PostgreSQL cleanup history
ATTEMPT_UNKNOWN
CORRUPT_RECORD
```

The Redis read API cannot return `CLEANED` after its maps are removed; the
dispatcher combines Redis inspection with the durable PostgreSQL attempt and
cleanup rows. A retry of Claim with the same attempt ID invokes the same
derivation and returns the valid later lifecycle rather than `CORRUPT_RECORD`.
It never creates a second Claim. A new Claim records the prior attempt as
`superseded` only after validating the queued requeue binding.

### 8.4 CompleteProcessing and FailProcessing

Validation:

```text
validate types, schema, full token, TerminalAuthorization, and reverse mapping
if terminal_op_id equals authorization and terminal state/token/event match:
    return IDEMPOTENT_TERMINAL_OUTBOX
if terminal with another operation/target: return OPERATION_CONFLICT
if token differs from generation/attempt: return STALE_ATTEMPT
require state=processing, processing score, exact active/lease attempt, and no
  queued/dead membership
require authorization token equals active token, requeue fields are null, and
  canonical terminal event sequence is authorized by the committed PG outbox
```

The worker receives this authorization only after the PostgreSQL terminal row
and outbox commit succeeds. On queued/requeued state these methods always return
`STATE_CONFLICT`, even when the token is the last requeued token. Thus a delayed
worker cannot terminalize queued work. Missing or corrupt attempt binding is
ambiguous and goes to Quarantine; Phase 1 has no RepairAttemptBinding.

Mutation order:

```text
HSET state completed, terminal_attempt token.attempt_id,
     terminal_op_id authorization.operation_id,
     terminal_event_seq authorization.event_seq
HSET attempt_outcome token.attempt_id completed
HSET attempt_operation token.attempt_id authorization.operation_id
HDEL active_attempt, lease_attempt, ready_seq
ZREM processing delivery_id
return APPLIED_COMPLETED
```

FailProcessing uses the same validation with closed disposition
`failed|dead-letter`, sets `attempt_outcome`, and never changes an existing
terminal outcome. It removes only processing membership and adds exact dead
membership for dead-letter.

### 8.5 ApplyTerminalOutbox for a requeued delivery

This API is available only to the PostgreSQL outbox dispatcher, not the worker.
Input is `TerminalAuthorization`. PostgreSQL commits the terminal row, a unique
terminal operation ID, monotonic event sequence, exact token, and expected
requeue operation binding in one transaction.

Validation before mutation:

```text
validate all key types, schema, UUIDs, target, canonical uint53 event sequence
validate attempt reverse mapping and generation
if terminal_op_id equals input and stored target/token/event sequence match:
    return IDEMPOTENT_TERMINAL_OUTBOX
if terminal and another operation/target is stored: return OPERATION_CONFLICT
if current generation is newer: return SUPERSEDED
require state=queued and exact queued score/ready_seq
require requeue_op_id/kind/attempt/generation all equal authorization
require authorization event_seq > requeue_event_seq
require no processing/dead membership and no active/lease attempt
```

Mutation order:

```text
HSET terminal_op_id, terminal_event_seq, terminal_attempt, state=target
HSET attempt_outcome attempt_id target, attempt_operation attempt_id operation_id
HDEL ready_seq and all requeue operation fields
ZREM queued delivery_id
if target=dead-letter: ZADD dead enqueue_seq delivery_id
return APPLIED_TERMINAL_OUTBOX
```

Redis cannot authenticate PostgreSQL or distinguish one permitted EVALSHA from
another by application intent. Phase 1 therefore does not claim per-script Redis
ACL authorization. ApplyTerminalOutbox is reachable only through the trusted Go
dispatcher interface/process described in section 11.1. Operation binding and
event ordering make its use auditable and stale operations non-mutating.

### 8.6 Recover — lease-driven only

`MAX_RECOVERY_ATTEMPTS` defaults to 3 for Phase 1. Inputs include exact observed
token, `RequeueOperation{kind=recover}`, and the configured limit. There is no
caller `now` argument.

Validation:

```text
validate types, schema, token, operation UUID/kind/reason/event_seq, record,
  attempt mappings, recovery_count and configured limit as canonical uint53
if queued and complete stored requeue binding equals the input:
    return IDEMPOTENT_RECOVER
if queued and requeue_attempt equals token but operation ID or kind differs:
    return ALREADY_REQUEUED_BY_OTHER_OPERATION
if terminal and attempt_operation equals this operation ID and terminal outcome
  is this Recover limit decision: return IDEMPOTENT_REQUEUE_DEAD_LETTER
if terminal by another operation: return SUPERSEDED with actual terminal outcome
if a newer generation exists: return SUPERSEDED
if token is not current active attempt: return STALE_ATTEMPT
require state=processing and no queued/dead membership
read processing ZSCORE and lease_attempt
TIME exactly once; validate/convert reply to redis_now_ms as section 3.1
if score and lease attempt are valid, match token, and score > redis_now_ms:
    return NOT_STALE
if score <= redis_now_ms: reason=EXPIRED
if processing membership/score missing: reason=MISSING_LEASE
if score is non-finite/out-of-policy: reason=MALFORMED_LEASE
if lease_attempt missing: reason=MISSING_LEASE_BINDING
if lease_attempt differs: reason=INCONSISTENT_LEASE
if processing membership is missing:
    require ZCARD(queued)+ZCARD(processing) < max_active before requeue
validate next_seq can increment
```

State=processing with missing processing membership is allowed only on this
repair/recovery path when the exact active token and PostgreSQL token agree.
Otherwise it is `CORRUPT_RECORD` and requires reconciliation/quarantine.
If its authoritative index slot has already been consumed by other work, Recover
returns `QUEUE_FULL` without mutation and the reconciler retries after capacity
opens; it never silently exceeds 500 or discards the orphan.

Poison decision and mutation:

```text
new_recovery_count = recovery_count + 1  # validated exact uint53
if new_recovery_count >= MAX_RECOVERY_ATTEMPTS:
    HSET state dead-letter, recovery_count, terminal_attempt token.attempt_id
    HSET attempt_outcome token.attempt_id dead
    HSET attempt_operation token.attempt_id operation_id
    HDEL active_attempt, lease_attempt, ready_seq
    ZREM processing delivery_id             # no-op when lease membership missing
    ZADD dead enqueue_seq delivery_id
    return APPLIED_DEAD_LETTER_RECOVERY_LIMIT

new_ready_seq = next_seq + 1
SET next_seq new_ready_seq
ZADD queued new_ready_seq delivery_id       # preserve before processing removal
HSET state queued, ready_seq new_ready_seq,
     recovery_count new_recovery_count,
     requeue_op_id operation_id, requeue_op_kind recover,
     requeue_attempt token.attempt_id, requeue_generation token.generation,
     requeue_reason reason, requeue_event_seq event_seq
HSET attempt_outcome token.attempt_id recovered
HSET attempt_operation token.attempt_id operation_id
HDEL active_attempt and lease_attempt
ZREM processing delivery_id
return APPLIED_RECOVERED(reason)
```

The crashing delivery moves to the tail on each recovery and deterministically
dead-letters on the third recovery. It cannot monopolize the queue head.

### 8.7 ReleaseClaim — DB Start rejection/rollback

ReleaseClaim is used only when PostgreSQL Start is definitively not applied, or
an unknown SQL outcome has been queried and the DB row is still queued. It takes
`RequeueOperation{kind=release}`. It does not inspect lease age; it increments
`release_count` exactly once and never increments `recovery_count`.

Phase 1 defaults `MAX_RELEASE_ATTEMPTS=5`. Allowed transient reasons are
`db_start_not_applied_transient` and `worker_shutdown_before_db_start`.
`db_start_permanent_inconsistency` is not releasable: PostgreSQL creates an
audited Quarantine operation. Every release operation has a PostgreSQL audit
row; if the limit is reached the same operation atomically dead-letters and its
audit outcome records `release_limit`.

Validation:

```text
validate types, schema, exact operation fields, active token, reverse mapping,
  release_count, configured limit, and canonical uint53 event sequence
if queued with complete stored requeue binding equal input: return IDEMPOTENT_RELEASE
if queued with same requeue attempt but another operation/kind:
    return ALREADY_REQUEUED_BY_OTHER_OPERATION
if terminal and attempt_operation equals this operation ID and terminal outcome
  is this Release limit decision: return IDEMPOTENT_REQUEUE_DEAD_LETTER
if terminal by another operation: return SUPERSEDED with actual terminal outcome
if newer generation: return SUPERSEDED
require state=processing, exact processing membership, active/lease binding,
  no queued/dead membership
validate closed transient release reason, next_seq, and release_count increment
```

Mutation order:

```text
new_release_count = release_count + 1
if new_release_count >= MAX_RELEASE_ATTEMPTS:
    HSET state dead-letter, release_count, terminal_attempt token.attempt_id
    HSET attempt_outcome token.attempt_id dead
    HSET attempt_operation token.attempt_id operation_id
    HDEL active_attempt, lease_attempt, ready_seq
    ZREM processing delivery_id
    ZADD dead enqueue_seq delivery_id
    return APPLIED_DEAD_LETTER_RELEASE_LIMIT
new_ready_seq = next_seq + 1
SET next_seq new_ready_seq
ZADD queued new_ready_seq delivery_id
HSET state queued, ready_seq new_ready_seq,
     release_count new_release_count,
     requeue_op_id operation_id, requeue_op_kind release,
     requeue_attempt token.attempt_id, requeue_generation token.generation,
     requeue_reason reason, requeue_event_seq event_seq
HSET attempt_outcome token.attempt_id released
HSET attempt_operation token.attempt_id operation_id
HDEL active_attempt and lease_attempt
ZREM processing delivery_id
return APPLIED_RELEASED(reason)
```

Replay of the same complete operation binding does not increment the count.
Recover/release limits bound successful requeues and therefore bound the number
of attempt mappings created before terminalization/Cleanup.

### 8.8 RecoveryCandidates and reconciliation scans

Normal stale discovery is deterministic and deadline ordered. The read-only
RecoveryCandidates Lua script takes no caller time, calls TIME exactly once,
validates it, then executes:

```text
ZRANGEBYSCORE processing -inf redis_now_ms WITHSCORES
```

The full expired set is at most 500 because processing shares the active hard
bound. Each returned member is expanded into a token and passed to Recover.
Items with equal deadlines use Redis member lexicographic tie order. A corrupt
early member therefore cannot hide a later expired member; both are returned in
the same bounded read. Recoverable members progress independently, while corrupt
members enter the reconciliation/quarantine path.

Deadline queries cannot find structural orphans, so reconciliation performs two
cursor-preserving audits:

1. HSCAN `state` with a cursor persisted in PostgreSQL, verifying every
   processing record has one processing ZSET member, participant/submission
   metadata, active attempt, reverse attempt mapping, generation, and lease
   binding.
2. ZSCAN `processing` with a separately persisted cursor, verifying every member
   has state=processing and complete token metadata.

Each audit continues from its stored cursor until Redis returns cursor 0, then
starts a new epoch. It never restarts at zero on every tick.

Detections and actions:

- state=processing but no processing member: if PostgreSQL stores the same exact
  token and Recover operation authorization, invoke Recover with reason
  MISSING_LEASE; otherwise quarantine/alert.
- processing member with missing state/participant/submission: do not mutate;
  create PostgreSQL quarantine audit/outbox using DB ownership evidence.
- missing/corrupt active or reverse attempt binding is ambiguous in Phase 1 and
  always creates an audited Quarantine operation. There is no
  RepairAttemptBinding operation.
- wrong Redis key type: stop the audit epoch, emit a high-severity alert, and
  make no mutation until the key type is restored from snapshot/reconciliation.

### 8.9 Audited Quarantine

Audit storage is PostgreSQL, not a Redis Stream:

```text
queue_audit_events(
  operation_id UUID PRIMARY KEY,
  delivery_id, submission_id, participant_id,
  observed_state, reason_code, evidence_json,
  created_at, retain_until, redis_applied_at
)
```

The audit row and a per-delivery Quarantine outbox event commit together. The
Lua script requires `operation_id`, validates any source membership and trusted
metadata, checks `quarantine_op`, then performs exact index removal and adds the
delivery to dead. It stores `quarantine_op[delivery_id]=operation_id`.

Retry with the same operation ID is idempotent. A different operation ID for an
already quarantined delivery returns `AUDIT_CONFLICT`. Audit rows are retained
at least through the security audit window and are deleted only by the same
retention policy that authorizes Cleanup.

### 8.10 Coordinated Cleanup with partial retry

Cleanup is a PostgreSQL-authorized outbox operation with a unique
`cleanup_operation_id`. The caller may dispatch it only when:

- PostgreSQL is terminal;
- all earlier per-delivery outbox events are delivered or explicitly superseded;
- retention/audit `retain_until` has elapsed;
- Redis has no active attempt and no queued/processing membership;
- the Redis state is completed, failed, or dead-letter.

The marker lives in fixed `cleanup_op`/`cleanup_phase` hashes, outside fields
deleted for the delivery. The attempts SET is the durable to-do list and is
deleted only after all reverse mappings. Cleanup has two modes.

First cleanup validation, before its first write:

```text
validate all fixed/dynamic key types and canonical authorization event sequence
require cleanup_op absent
require terminal/dead state, no active attempt, no queued/processing membership
require exact PostgreSQL terminal/quarantine authorization
read SMEMBERS attempts:<delivery_id>
for every member require each remaining attempt_delivery equals delivery_id and
  attempt_generation is canonical; missing mapping is allowed only when the
  durable PostgreSQL cleanup manifest marks that attempt already removed
```

First mutation is `HSET cleanup_op operation_id, cleanup_phase started`. Any
later unexpected failure is outcome-unknown and leaves the marker for resume.

Resume validation:

```text
if cleanup_op differs: return OPERATION_CONFLICT
if cleanup_op equals input: accept phase started|reverse_maps|attempt_set|record|verified
if cleanup_op equals input and cleanup_phase is absent: enter finalizing mode;
  require the complete manifest and delivery record/index absence, then remove
  cleanup_op as the last remaining marker
require remaining live memberships are absent
for every attempt still in attempts SET:
    remaining reverse maps must be absent or exactly match this delivery/gen
    a reverse map owned by another delivery is CLEANUP_CONFLICT
missing already-deleted mappings and record fields are accepted
```

Idempotent mutation phases, in order:

```text
HSET cleanup_phase reverse_maps
for each attempt still listed:
    HDEL attempt_delivery, attempt_generation, attempt_outcome, attempt_operation
HSET cleanup_phase attempt_set
DEL attempts:<delivery_id>               # only after all reverse maps
HSET cleanup_phase record
ZREM dead delivery_id
HDEL all delivery fields: state, submission, participant, enqueue_seq,
  ready_seq, generation, attempt_count, recovery_count, release_count,
  active_attempt, lease_attempt, every requeue field, every terminal field,
  quarantine_op
HSET cleanup_phase verified
verify all listed delivery fields, dead/queued/processing memberships,
  attempts SET, and manifest reverse mappings are absent
HDEL cleanup_phase delivery_id
HDEL cleanup_op delivery_id               # marker removal is last
return APPLIED_CLEANUP
```

The post-delete verification has no expected error result: validation has
already established ownership and the deletes are idempotent. An unexpected
command/runtime failure at or after it is outcome-unknown and leaves the marker
unless marker removal itself was the unknown command.

If a reply is lost after marker removal, the same PostgreSQL operation plus
full absence returns `ALREADY_CLEAN`. A different operation ID conflicts until
or after completion according to the durable PostgreSQL cleanup row. Cleanup
never recreates missing fields. Fault-injection tests stop after the marker,
mid reverse-map deletion, SET deletion, dead removal, record deletion,
verification, between phase-marker and operation-marker deletion, and after
marker deletion, then resume the same operation.

## 9. Result and error codes

| Code | Name | Mutation | Meaning |
|---:|---|---:|---|
| 0 | `APPLIED` | yes | requested transition applied |
| 1 | `IDEMPOTENT` | no | same operation already applied |
| 2 | `ALREADY_COMPLETED` | no | completed terminal result |
| 3 | `ALREADY_FAILED` | no | failed terminal result |
| 4 | `ALREADY_DEAD` | no | dead-letter terminal result |
| 5 | `STALE_ATTEMPT` | no | older/different token |
| 6 | `NO_JOB` | no | queued index empty |
| 7 | `QUOTA_BLOCKED` | no | full scan found no eligible participant |
| 8 | `NOT_STALE` | no | Recover lease remains valid |
| 9 | `IDEMPOTENT_EXISTING` | no | existing record and indexes fully valid |
| 10 | `IDEMPOTENT_RELEASE` | no | same ReleaseClaim operation binding already applied |
| 11 | `ALREADY_CLEAN` | no | authorized cleanup already absent |
| 12 | `IDEMPOTENT_RECOVER` | no | same Recover operation binding already applied |
| 13 | `ALREADY_REQUEUED_BY_OTHER_OPERATION` | no | same attempt was requeued by another ID/kind |
| 14 | `SUPERSEDED` | no | later generation/event has replaced this operation |
| 15 | `CLAIMED_PROCESSING` | no | retry/inspection sees attempt processing |
| 16 | `REQUEUED_BY_RECOVER` | no | retry/inspection sees Recover outcome |
| 17 | `REQUEUED_BY_RELEASE` | no | retry/inspection sees Release outcome |
| 18 | `IDEMPOTENT_TERMINAL_OUTBOX` | no | same terminal authorization already applied |
| 19 | `IDEMPOTENT_REQUEUE_DEAD_LETTER` | no | same Recover/Release operation caused limit dead-letter |
| -10 | `WRONG_TYPE` | no | Redis key type invalid |
| -11 | `INVALID_ARGUMENT` | no | argument/range/enum invalid |
| -12 | `SCHEMA_MISMATCH` | no | namespace is not v3 |
| -13 | `DELIVERY_CONFLICT` | no | immutable metadata differs |
| -14 | `STATE_CONFLICT` | no | transition/state disagree |
| -15 | `CORRUPT_RECORD` | no | record/index/token invariant broken |
| -16 | `QUEUE_FULL` | no | queued plus processing is 500 |
| -17 | `QUEUED_QUOTA` | no | authoritative queued scan reaches limit |
| -18 | `LEASE_CONFLICT` | no | queued record unexpectedly owns a lease |
| -19 | `ATTEMPT_COLLISION` | no | attempt ID owned elsewhere |
| -20 | `SEQUENCE_EXHAUSTED` | no | safe sequence range exhausted |
| -21 | `RECOVERY_LIMIT` | no | invalid configured recovery limit |
| -22 | `AUDIT_CONFLICT` | no | quarantine operation ID conflicts |
| -23 | `CLEANUP_PRECONDITION` | no | cleanup safety condition not met |
| -24 | `OPERATION_CONFLICT` | no | operation ID/kind/reason/event binding differs |
| -25 | `NUMERIC_EXHAUSTED` | no | a canonical uint53 value cannot increment |
| -26 | `RELEASE_LIMIT` | no | configured release limit is invalid |
| -27 | `CLEANUP_CONFLICT` | no | remaining cleanup mapping belongs elsewhere |
| -28 | `AUTHORIZATION_CONFLICT` | no | terminal/outbox authorization does not match |
| -29 | `ATTEMPT_UNKNOWN` | no | no Redis or durable PostgreSQL attempt evidence |
| -30 | `ATTEMPT_ALLOCATOR_EXHAUSTED` | no | PostgreSQL allocator cannot issue another non-reused ID |
| -31 | `REDIS_TIME_INVALID` | no | TIME reply malformed or outside canonical uint53 milliseconds |
| -32 | `LEASE_POLICY_INVALID` | no | duration or hard-timeout/safety invariant invalid |

There is no counter-related error because counters are not lifecycle state.
Unexpected Redis errors are not mapped to an applied/validation code; they are
reported as outcome-unknown.

## 10. Duplicate, quota, and fairness semantics

- Duplicate detection checks state-specific invariants before returning
  idempotent success.
- Queued quota is the count of queued ZSET members whose authoritative
  participant field matches.
- Running quota is the count of processing ZSET members whose participant field
  matches.
- Claim builds running counts once and scans every queued member in ready order.
- ZSET `ZREM delivery_id` cannot remove another job with equal metadata.
- Recovery/Release goes to tail; recovery and release limits prevent crashing or
  DB-start-rejected jobs from cycling forever.
- Telemetry counts are derived, disposable, and rebuilt from ZSETs; negative
  counter invariants no longer exist.

## 11. Safe API and current caller compatibility

Safe API:

```text
Enqueue(ctx, Delivery, Limits) (EnqueueResult, error)
Claim(ctx, attemptID) (*ClaimedJob, error) # queue owns validated lease/quota policy
InspectAttempt(ctx, attemptID) (AttemptSnapshot, error)
CompleteProcessing(ctx, ClaimToken, TerminalAuthorization) (TerminalResult, error)
FailProcessing(ctx, ClaimToken, TerminalAuthorization) (TerminalResult, error)
ApplyTerminalOutbox(ctx, TerminalAuthorization) (TerminalResult, error)
RecoveryCandidates(ctx) ([]RecoveryCandidate, error)
Recover(ctx, RequeueOperation, maxRecoveries) (RecoverResult, error)
ReleaseClaim(ctx, RequeueOperation, maxReleases) (ReleaseResult, error)
InspectDelivery(ctx, deliveryID) (DeliverySnapshot, error)
Quarantine(ctx, QuarantineAuthorization) (QuarantineResult, error)
Cleanup(ctx, CleanupAuthorization) (CleanupResult, error)
Depth/ProcessingLen/DeadLen(ctx)
```

Compatibility decisions:

- `EnqueueCheck` may temporarily adapt submission ID to delivery ID.
- Existing HTTP mapping remains: active queue saturation -> 503, queued quota ->
  409. `QUEUE_MAX_DEPTH` remains 500.
- `JobItem` grows a ClaimToken.
- Old Complete, Fail, and RecoverOne signatures cannot remain mutating APIs;
  worker call sites must migrate at compile time to tokens. Compile-time Go
  interfaces prevent accidental privileged dispatch but are not described as a
  security boundary against compromise of a trusted worker process.
- `RecoverStale` becomes read-only deadline candidate discovery.
- DB Start definitive rejection uses ReleaseClaim, never Recover or Fail.
- `cmd/mock-worker` must store the Claim token in PostgreSQL Start CAS. After a
  terminal DB/outbox commit it may dispatch that exact authorization through
  processing-only Complete/Fail; queued convergence is exclusively handled by
  ApplyTerminalOutbox with the same durable authorization.
- `internal/submissions` must replace create-as-queued/direct-Enqueue cleanup
  with the durable enqueue protocol below. This is an intentional compile-time
  migration; the clean legacy API is not semantically compatible.
- `internal/config` must add the four duration settings in section 3.1 and reject
  invalid relationships plus the four outbox retry settings in section 12.4;
  current `MockWorkDuration` alone is insufficient.
- Current Compose has one shared internal network, no Redis authentication, and
  no dispatcher service. It is implementation evidence of the gap, not evidence
  for this boundary; the implementation checkpoint must add the trusted-data/
  untrusted separation, credential mounts, and dispatcher before acceptance.

### 11.1 Phase 1 trusted-process capability boundary

Phase 1 chooses the trusted process boundary, not per-Lua-script ACLs:

```text
APIQueue interface:
  Enqueue, InspectDelivery, depth reads

WorkerQueue interface:
  Claim, InspectAttempt, CompleteProcessing, FailProcessing

DispatcherQueue interface (trusted dispatcher only):
  InspectDelivery, InspectAttempt, RecoveryCandidates,
  Recover, ReleaseClaim, ApplyTerminalOutbox, Quarantine, Cleanup
```

The raw Redis client and script handles stay private to queue package
construction. `cmd/mock-worker` receives only WorkerQueue; dispatcher-only
methods are neither on that interface nor passed to worker code. A separate
trusted `queue-dispatcher` Go service owns PostgreSQL outbox dispatch and
reconciliation. This separation prevents accidental misuse and makes review
boundaries explicit, but both Go services remain trusted infrastructure.

Redis authentication material is mounted/provided only to trusted Go services:
API, mock-worker, dispatcher, and the rate-limit component. Submitted source is
inert Phase 1 data and is never executed. No submitted/untrusted process receives
Redis credentials, Redis address environment, a mounted secret, a Redis socket,
an inherited Redis connection/file descriptor, or a network route to the Redis
network. Trusted code never spawns a submitted process in Phase 1.

Deployment uses separate networks:

```text
front:         nginx <-> api
trusted-data:  api, mock-worker, queue-dispatcher <-> PostgreSQL/Redis
untrusted:     negative-test probe only in Phase 1; future execution services
```

Redis and PostgreSQL are not attached to `untrusted` and are not published to
the host. The untrusted probe has no shared network with trusted-data. The API
may bridge front/trusted-data because it is trusted and never executes source.

Redis ACLs still require authentication, restrict trusted credentials to the v3
and rate-limit key prefixes, and deny administrative/dangerous commands such as
FLUSHALL, FLUSHDB, CONFIG, MODULE, DEBUG, SHUTDOWN, and unrestricted key scans.
Script installation/loading is a deployment/bootstrap action. Runtime services
may invoke the required script mechanism, but ACLs are explicitly **not** a
worker-versus-dispatcher or per-script authorization boundary: a compromised
trusted service with script capability is inside the trusted computing base.

Acceptance tests inspect rendered Compose/container configuration without
printing secrets, assert only trusted services receive the credential mount and
trusted-data attachment, and start an ephemeral process on `untrusted` that
cannot resolve or connect to Redis TCP. A second test verifies it has no Redis
environment, credential mount, Unix socket, or inherited descriptor. Phase 1
also asserts no source execution/spawn path exists. Phase 2 must preserve this
network/credential/descriptor isolation when it introduces an actual untrusted
runtime.

## 12. PostgreSQL outbox and reconciliation boundary

Redis and PostgreSQL cannot share one atomic transaction. Required schema:

```text
submissions.queue_delivery_id UUID UNIQUE
submissions.queue_attempt_id TEXT nullable
submissions.queue_generation BIGINT nullable CHECK 0 <= value <= 2^53-1
submissions.status in
  pending_enqueue|enqueue_unknown|queued|mock_processing|
  enqueue_rejected|finished|internal_error
submissions.enqueue_rejection_reason nullable
submissions.enqueue_http_status nullable
submissions.queue_terminal_operation_id UUID nullable
submissions.queue_terminal_reason nullable

queue_attempt_allocator(deployment_epoch UUID PRIMARY KEY,
                        next_serial BIGINT NOT NULL) # non-recycling
queue_attempts(attempt_id TEXT PRIMARY KEY, delivery_id nullable, generation nullable,
               lifecycle_outcome, requeue_operation_id, terminal_operation_id,
               created_at, retain_until)
queue_outbox(id UUID PRIMARY KEY, delivery_id, attempt_id, generation,
             event_seq BIGINT CHECK uint53, operation, payload,
             created_at, delivered_at, superseded_at,
             retry_count, next_attempt_at, last_error_code,
             decision_required_at,
             superseded_reason, superseded_by_operation_id,
             UNIQUE(delivery_id,event_seq))
queue_outbox_decisions(decision_id UUID PRIMARY KEY, event_id UUID,
                       action delivered|superseded|quarantine,
                       evidence_json, decided_by, created_at)
queue_audit_events(operation_id UUID PRIMARY KEY, delivery identity/token,
                   reason_code, evidence_json, redis_outcome,
                   redis_applied_at, db_converged_at, retain_until)
queue_reconcile_cursors(index_name PRIMARY KEY, cursor, epoch, updated_at)
source_cleanup_tasks(operation_id UUID PRIMARY KEY, delivery_id, storage_key,
                     not_before, completed_at)
queue_cleanup_operations(operation_id UUID PRIMARY KEY, delivery_id,
                         manifest_json, retain_until, redis_completed_at)
```

### 12.1 Durable Enqueue and HTTP behavior

1. One DB transaction creates the submission as `pending_enqueue`, retains the
   committed source, and creates Enqueue outbox event E with stable delivery ID.
2. Synchronous dispatch invokes E only through the same ordered algorithm in
   section 12.4; it cannot bypass an older pending event for that delivery.
3. `APPLIED`/valid `IDEMPOTENT_EXISTING`: a DB transaction conditionally changes
   `pending_enqueue|enqueue_unknown -> queued` and marks E delivered. HTTP 202
   returns truthful `queued`.
4. Definitive `QUEUE_FULL`: one DB transaction changes to `enqueue_rejected`,
   stores reason/HTTP 503, supersedes E, and creates a durable source-cleanup
   task. Definitive `QUEUED_QUOTA` does the same with HTTP 409. Source deletion
   occurs only after this commit and its configured retention policy.
5. Transport timeout or unexpected Redis runtime failure: one DB transaction
   changes `pending_enqueue -> enqueue_unknown`; E stays undelivered and source
   stays retained. HTTP 202 returns the truthful `enqueue_unknown` status and
   opaque submission ID for polling--never `queued`, 409, or 503 by guess.
6. Reconciler locks the delivery, InspectDelivery, then retries the same E when
   absent; valid queued/processing/terminal state proves Enqueue applied;
   definitive retry rejection commits `enqueue_rejected`; corruption creates an
   audit/Quarantine event. A worker that Claims before DB reaches queued cannot
   Start; it emits exact ReleaseClaim and does no work.

### 12.2 Claim, Start, requeue, and terminal protocol

1. Before Redis Claim, the worker inserts a unique `claim_reserved`
   queue_attempts row. Redis Claim returns a token and Claim retry uses
   InspectAttempt semantics. In one DB transaction the reservation is bound to
   delivery/generation and Start CAS changes only `queued -> mock_processing`
   while storing that exact token. NO_JOB/QUOTA_BLOCKED closes the reservation;
   outcome-unknown keeps it reserved for inspection/retry. Closed reservation
   rows may later be retained/cleaned, but the allocator never reissues the ID.
2. If DB Start definitively affects zero rows, query the row:
   - queued/pending/unknown with no applied Start: commit a uniquely identified
     transient ReleaseClaim outbox event;
   - processing with same token: continue;
   - processing with another token: mark attempt superseded; do not mutate Redis;
   - terminal: commit exact TerminalAuthorization event;
   - permanent identity/state inconsistency: commit audit plus Quarantine event.
3. Unknown DB Start is queried until one case is known. Recover never undoes a
   fresh Claim. Release events include operation ID, kind, reason, token, and
   event sequence; delivered status requires the matching idempotent result.
   `ALREADY_REQUEUED_BY_OTHER_OPERATION`, `OPERATION_CONFLICT`, a generic
   terminal outcome, or `SUPERSEDED` does not mark this event delivered; the
   dispatcher inspects and supersedes only with durable proof of the winner.
   Only the exact same operation's applied/idempotent result, including
   `IDEMPOTENT_REQUEUE_DEAD_LETTER`, marks it delivered.
4. Every applied Release increments Redis release_count once. PostgreSQL keeps
   an audit row for each release. At the configured limit Redis dead-letters;
   dispatcher records `release_limit` and terminalizes the DB row. Permanent DB
   inconsistency uses Quarantine immediately rather than consuming retries.
5. Stale recovery transaction R1 locks the submission/attempt, verifies the
   exact token, conditionally changes DB `mock_processing -> queued`, and inserts
   one Recover outbox/audit operation with reason and event sequence. The
   dispatcher invokes Recover with that exact operation. Missing/malformed/
   expired lease is recoverable only with unambiguous attempt binding;
   ambiguity creates Quarantine instead.
6. DB Finish/Fail commits terminal state plus a unique ordered terminal outbox
   authorization. When Redis is still processing, processing-only Complete/Fail
   uses the exact token and that same authorization. When Redis is queued/requeued, only
   ApplyTerminalOutbox can remove it, and it requires exact requeue operation,
   terminal operation ID, token, and event sequence.
7. `event_seq` is per-delivery canonical uint53. Allocation exhaustion blocks a
   new operation before commit and alerts. Ordering uses the selection algorithm
   in section 12.3; an advisory lock alone is not treated as sufficient.
8. Reconciler compares DB rows, attempt lifecycle, Redis snapshots, and
   persistent index cursors. It never guesses from payload/participant and never
   issues compensating Redis removal after outcome-unknown.

### 12.3 Recovery-limit and Quarantine DB convergence

For `APPLIED_RECOVERED` or exact `IDEMPOTENT_RECOVER`, the ordered dispatcher
marks the Recover event delivered and DB remains queued. For
`APPLIED_DEAD_LETTER_RECOVERY_LIMIT` or
`IDEMPOTENT_REQUEUE_DEAD_LETTER` proven to be caused by the same Recover
operation, it continues the already-open ordered-dispatch transaction with R2
while holding the per-delivery advisory/row locks:

```text
lock submission, queue_attempt, Recover outbox, and audit row
require outbox operation_id, kind=recover, event_seq, and exact token match
UPDATE submissions
  SET status='internal_error',
      queue_terminal_reason='recovery_limit',
      queue_terminal_operation_id=:operation_id,
      finished_at=COALESCE(finished_at,NOW()), updated_at=NOW()
  WHERE id=:delivery_id AND status='queued'
    AND queue_attempt_id=:attempt_id AND queue_generation=:generation
if one row updated, or row is already internal_error with the same
  operation_id/reason/token:
    set queue_attempt lifecycle_outcome='dead'
    set audit redis_outcome='recovery_limit', redis_applied_at if absent,
      db_converged_at if absent
    mark this Recover outbox delivered_at if absent
    allow the outer dispatcher transaction to COMMIT
else:
    do not mark delivered; record retry/conflict evidence and let the outer
      dispatcher transaction COMMIT that evidence (or ROLLBACK on DB error)
```

Thus the earlier `mock_processing -> queued` recovery CAS is intentionally
reversed to terminal only by the exact operation that hit its limit. All writes
are idempotent through operation/token predicates and `COALESCE`/write-once
fields. A different operation's dead-letter result, a generic ALREADY_DEAD, or
a mismatched token never delivers the current event.

If the Recover response is lost, the dispatcher calls InspectAttempt and
InspectDelivery. Only `state=dead-letter`, `attempt_operation=operation_id`, and
the exact attempt/generation authorize R2. If inspection finds another winning
operation, section 12.4's audited supersede decision handles the current event;
otherwise it retries with backoff.

Quarantine begins with one PostgreSQL transaction that inserts the immutable
audit row and ordered Quarantine outbox event for an exact delivery/token (or
documented missing token evidence). Redis Quarantine stores the same
`quarantine_op`, removes only validated membership, and adds dead membership.
On `APPLIED` or exact same-operation idempotency, the dispatcher transaction:

```text
locks submission, Quarantine outbox, and audit row
CAS status in (pending_enqueue, enqueue_unknown, queued, mock_processing)
  -> internal_error, requiring the exact token when one exists or the audit's
  explicit missing-token evidence when it does not
set queue_terminal_reason='quarantine:' || reason_code
set queue_terminal_operation_id=operation_id and finished_at
set audit redis_outcome='quarantined', redis_applied_at, db_converged_at
mark the matching Quarantine outbox delivered
```

Replay accepts an already-internal_error row only when terminal operation,
reason, delivery, and token/evidence all match. Response loss is reconciled by
InspectDelivery proving dead-letter plus exact `quarantine_op`. Another audit
operation, missing proof, or conflicting PostgreSQL terminal operation does not
deliver the event. Audit evidence remains until the coordinated retention gate.

### 12.4 Actual ordered outbox dispatcher algorithm

Phase 1 configuration is startup validated:

```text
OUTBOX_RETRY_MIN=250ms
OUTBOX_RETRY_MAX=30s
OUTBOX_MAX_AUTOMATIC_RETRIES=20
OUTBOX_MAX_PENDING_AGE=15m
```

The retry durations are positive canonical milliseconds, min <= max, retry
count is positive, and pending age is positive. Redis dispatch uses the bounded
`REDIS_OPERATION_TIMEOUT` from section 3.1; the DB transaction timeout must be
greater than that timeout plus local acknowledgement budget.

A scheduler may choose a delivery ID as a non-authoritative work hint. For each
attempt the dispatcher executes exactly:

```text
BEGIN
SELECT pg_advisory_xact_lock(hash_delivery_id(:delivery_id))
SELECT * FROM queue_outbox
 WHERE delivery_id=:delivery_id
   AND delivered_at IS NULL
   AND superseded_at IS NULL
 ORDER BY event_seq ASC
 LIMIT 1
 FOR UPDATE
```

No later event is queried or dispatched. If no row exists, COMMIT. If the head
row's `next_attempt_at` is in the future, COMMIT without a Redis call; N+1 stays
blocked. Otherwise dispatch only that event with a bounded Redis request timeout
while the transaction-scoped advisory lock and row lock are held. Interpret the
result using that operation's exact idempotency rules, then in the same DB
transaction either mark it delivered, make an explicitly audited supersede
decision, or update retry metadata; finally COMMIT.

Concurrent dispatchers for one delivery serialize on the transaction-scoped
advisory lock. The post-lock `ORDER BY event_seq ... FOR UPDATE` is the authority,
so a scheduler race cannot select N+1. Dispatchers for different deliveries may
run concurrently.

Failure behavior:

- Crash/rollback before Redis call leaves the head event unchanged.
- Crash or DB connection loss after Redis call but before acknowledgement leaves
  it pending. Retry dispatches the same operation ID; if the response was also
  lost it inspects actual Redis state before deciding.
- A retryable transport/runtime/temporary dependency failure increments
  `retry_count`, stores a bounded error code (not secrets), and sets capped
  exponential `next_attempt_at`. It never selects N+1 during backoff.
- When another operation won, the current event is not marked delivered. After
  InspectDelivery/InspectAttempt plus matching durable audit/outbox evidence, a
  DB transaction records `superseded_at`, `superseded_reason`, and
  `superseded_by_operation_id`. That explicit decision unblocks N+1.
- A poison event cannot be silently dropped or block forever. After configured
  retry-count/age thresholds the transaction sets `decision_required_at`, inserts
  an alert, and stops automatic Redis calls for that head. N+1 remains blocked
  visibly until an idempotent `queue_outbox_decisions` row records one of three
  evidence-backed actions: prove applied and mark delivered, prove a winner and
  supersede, or create/authorize Quarantine and supersede the poison event in the
  same audited transaction. Mere retry exhaustion is not authority to supersede.

The dispatcher uses a finite Redis timeout shorter than its DB transaction
timeout. Transaction rollback never compensates Redis; outcome-unknown is
resolved by the same operation's inspection/idempotent retry. N+1 is eligible
only after N has a committed `delivered_at` or `superseded_at`.

### 12.5 Cleanup authorization

After PostgreSQL terminal/outbox/audit retention conditions hold, a transaction
creates one cleanup operation and an immutable manifest of every retained
attempt ID/generation. The same operation ID and manifest drive first/resume
Cleanup. Only after Redis reports applied/already-clean and inspection confirms
absence is the cleanup outbox marked delivered. PostgreSQL attempt/audit rows
remain through their own audit window, allowing InspectAttempt to explain a
cleaned attempt.

Non-atomic windows remain between Redis Claim and DB Start, between Redis
Enqueue and its DB acknowledgement, and between SQL outbox commit and Redis
application. Token/operation CAS, ReleaseClaim, ordered outbox, inspection, and
retry provide eventual convergence; this is not a distributed transaction.

## 13. Crash, timeout, and concurrency scenarios

| Scenario | Required result |
|---|---|
| Enqueue response lost | DB becomes enqueue_unknown, retains source/outbox; inspect/retry same delivery |
| Definitive Enqueue rejection | DB becomes enqueue_rejected; outbox superseded; durable source cleanup; 503/409 |
| Claim response lost, still processing | same attempt retry returns CLAIMED_PROCESSING |
| Claim response lost, then recovered/released | same attempt retry/InspectAttempt returns exact operation outcome |
| Claim response lost, then terminal/dead/new generation | returns terminal/dead/SUPERSEDED, never generic corruption |
| DB Start rejects fresh Claim | unique ordered ReleaseClaim event requeues without waiting for lease |
| Release loop | each distinct applied release increments once; configured limit dead-letters and audits |
| Worker dies | processing score expires; Recover requeues tail or dead-letters at limit |
| Workers have fast/slow wall clocks | no effect; Claim/candidate/Recover/Inspect lease decisions use Redis TIME |
| Claim/Inspect response is delayed | delay only consumes lease; insufficient Redis-derived remaining time causes Release before DB Start |
| Phase 1 work reaches hard timeout | cancellation precedes lease expiry; exact-token internal_error outbox or lease recovery converges |
| Missing processing membership | cursor audit detects; exact unambiguous DB token authorizes Recover |
| Processing orphan lacks state | ZSCAN audit creates quarantine audit; no blind removal |
| Ambiguous attempt binding | audited Quarantine; no Phase 1 repair operation |
| Complete/Fail response lost | retry exact processing token; terminal changes once |
| Recover/Release response lost | retry same operation ID/kind/token; count and requeue occur once |
| Recovery-limit Redis response lost | exact InspectAttempt dead/operation proof drives queued -> internal_error and delivers only matching event |
| Quarantine response lost | exact quarantine_op/dead proof drives internal_error and matching audit/outbox acknowledgement |
| Other requeue op follows winner | ALREADY_REQUEUED_BY_OTHER_OPERATION/OPERATION_CONFLICT; outbox not falsely delivered |
| Old terminal/release after new Claim | generation mismatch; new attempt untouched |
| Old worker terminal call after requeue | worker API returns STATE_CONFLICT; only authorized terminal outbox may apply |
| Wrong key type | validation/audit stops with zero intended mutation |
| Unexpected Lua/runtime error | outcome unknown; inspect/reconcile, never compensate |
| Two Claims same participant | scripts serialize; second derives quota from updated processing ZSET |
| Quota-blocked prefix | full scan reaches eligible delivery elsewhere in the <=500 queue |
| Recover versus Complete | scripts serialize; token/state determines one applied winner |
| Cleanup fails after any mutation | marker/phase persists; same operation resumes missing-safe; different ID conflicts |
| Cleanup response lost after marker removal | durable cleanup row plus full absence returns ALREADY_CLEAN |
| Numeric max reached | explicit NUMERIC_EXHAUSTED before mutation; no rounded generation/count |
| Two outbox dispatchers choose one delivery | transaction advisory lock then lowest pending row lock permits only event N |
| Dispatcher crashes before Redis | transaction rolls back; head event remains pending |
| Dispatcher crashes after Redis before DB ack | same operation retries/inspects; N+1 remains blocked |
| Poison outbox head | bounded backoff then explicit audited deliver/supersede/quarantine decision; never silent skip |

## 14. Legacy migration

Migration is offline; v3 never interprets legacy payloads during normal work:

1. Stop producers/workers and snapshot Redis plus PostgreSQL.
2. Validate old key types before reading.
3. Build empty v3 namespace.
4. Use PostgreSQL submission/participant columns for identity and metadata.
5. Assign first enqueue/ready sequences from legacy order evidence; never split
   payload to establish identity.
6. Rebuild processing entries as migration attempts with expired deadlines so
   tokenized reconciliation handles them.
7. Initialize canonical uint53 attempt/recovery/release counts from auditable
   evidence, otherwise zero; overflowed legacy values quarantine.
8. Put malformed/unknown legacy values through PostgreSQL quarantine audit.
9. Do not migrate counters; quotas derive from rebuilt indexes.
10. Verify every nonterminal DB row, all index/state invariants, and active total
    <= 500 before cutover.
11. Switch prefix atomically and keep legacy keys read-only through audit window.

No dual-write mode is allowed.

## 15. Test plan before implementation

Required Redis tests use a fresh real Redis 7 address and fail rather than skip
when Redis is unavailable.

### Starvation, FIFO, and performance

- Fill the first 499 ready positions with quota-blocked participants and place
  one eligible delivery at position 500; Claim must select it.
- Repeat with mixed participants and prove lowest eligible `ready_seq` wins.
- Assert Enqueue rejects active item 501 and configuration rejects depth >500.
- Benchmark worst-case O(P+Q) Claim and Enqueue Lua at P+Q=500.
- Recover/Release moves a delivery to tail while preserving `enqueue_seq`.

### Authoritative quota/index tests

- Queued quota derives only from queued members and participant metadata.
- Running quota derives only from processing members and participant metadata.
- Corrupt or absent telemetry caches have no effect.
- State/index mismatch returns CORRUPT_RECORD before mutation.
- Concurrent Claims for one participant never exceed max running.

### Recovery and reconciler tests

- RecoveryCandidates returns the complete bounded expired set in deadline order.
- A corrupt earliest member does not hide any later expired member.
- Persistent HSCAN/ZSCAN cursors complete an epoch without restarting.
- Detect state-processing/missing membership, orphan membership/missing state,
  missing participant, missing/corrupt active attempt, reverse mapping mismatch,
  lease binding mismatch, non-finite/out-of-policy score, and wrong key type.
- Prove the exact DB token repairs/recoveries and ambiguous evidence quarantines.

### Redis lease authority and hard timeout

- Run Claim with worker clocks artificially years fast and slow; identical Redis
  TIME produces identical deadline semantics and no API accepts caller `now`.
- Validate TIME seconds/microseconds canonical conversion, microsecond floor,
  malformed shapes/strings, microseconds 1000000, uint53 boundary, and deadline
  addition overflow; every failure precedes mutation.
- RecoveryCandidates and Recover on opposite-skew workers agree at deadline-1,
  deadline, and deadline+1 according to Redis TIME; Recover rechecks after delay.
- Delay/drop Claim and Inspect replies; the worker starts only with
  Redis-derived remaining >= hard timeout+safety and otherwise ReleaseClaims.
- Reject zero/negative/fractional-ms/overflow durations, work duration above hard
  timeout, lease below hard+safety, and Redis timeout >= safety at startup.
- Force mock work past `MOCK_WORK_HARD_TIMEOUT`; cancellation and exact-token DB
  terminal/outbox happen before Redis lease expiry. If DB is unavailable, no
  blind Redis mutation occurs and expiry recovery succeeds.

### ReleaseClaim and attempt safety

- Fresh unexpired lease can be released after DB Start rejection.
- Same operation Release twice is idempotent and increments release_count once.
- Recover then replay Release and Release then replay Recover return the exact
  other-operation/conflict outcome and do not mark the losing outbox delivered.
- Same operation ID with a different kind/reason/token/event is OPERATION_CONFLICT.
- Distinct Releases reach MAX_RELEASE_ATTEMPTS, then atomically dead-letter with
  one PostgreSQL audit outcome; permanent inconsistency quarantines immediately.
- Release after generation N+1 is SUPERSEDED.
- DB Start unknown then queried queued emits ordered ReleaseClaim outbox event.
- Recover still returns NOT_STALE for the same fresh lease.
- Delayed Complete/Fail/Recover/Release of N never changes N+1.

### Claim retry and uint53

- Lose Claim response, then inspect/retry while processing, recovered, released,
  completed, failed, dead-letter, and after generation N+1; assert each distinct
  lifecycle result and no mutation.
- Inspect a cleaned attempt through durable PostgreSQL cleanup history while
  its retention window remains; after audit deletion return ATTEMPT_UNKNOWN.
- Attempt allocator reservation is globally unique; an ID retained in PostgreSQL
  after Redis Cleanup cannot be reserved or used for a new Claim.
- Attempt allocator max-1/max behavior is explicit; exhaustion creates no
  reservation and makes no Redis call.
- Parse 0, 1, 2^53-1 canonically; reject sign, leading zero (except `0`), space,
  exponent, fraction, NaN, 2^53, and rounded aliases.
- Generation, attempt_count, recovery_count, release_count, sequence, event_seq,
  time, and deadline each test max-1 increment, max exhaustion, no mutation.

### Poison policy

- First and second expired recovery move the item to tail.
- Recovery at configured limit atomically dead-letters it.
- ReleaseClaim increments only release_count and Recover only recovery_count.
- Recovery retry does not increment twice.
- Other eligible jobs continue to claim while poison delivery cycles.

### Enqueue and cleanup

- Existing valid state in every state returns correct idempotent/terminal result.
- State queued without queued membership, wrong score, cross-index duplicate,
  missing terminal attempt, or missing immutable field returns CORRUPT_RECORD.
- Cleanup rejects nonterminal, active, indexed, unexpired-retention, or
  undelivered-outbox cases.
- Definitive QUEUE_FULL/QUEUED_QUOTA produce DB enqueue_rejected, 503/409,
  superseded outbox, retained-then-cleaned source; Redis unknown produces
  enqueue_unknown, 202 truthful status, retained source, same-delivery retry.
- Crash after source commit, submission/outbox commit, Redis application, DB
  acknowledgement, rejection commit, and source-cleanup scheduling converges.
- Cleanup removes every listed record field, dead membership, per-delivery
  attempt set, and reverse attempt maps; retry returns ALREADY_CLEAN.
- Fault inject after cleanup marker, each reverse-map deletion, attempts SET,
  dead index, each record-field group, verified phase, and marker removal. Same
  operation resumes; different operation conflicts; foreign mapping is never
  deleted.

### Audit and outcome-unknown

- Quarantine creates one PostgreSQL audit row/outbox operation; Redis retry with
  same ID is idempotent and different ID conflicts.
- Audit retention blocks Cleanup until `retain_until`.
- Drop response after every mutation phase; inspect by delivery/token and retry
  without compensating removal.
- Inject unexpected script/runtime/transport failures and prove reconciler
  determines actual state.
- Recovery-limit applied and response-lost paths CAS DB queued -> internal_error
  with reason, operation and token; retry does not rewrite terminal evidence.
- A dead-letter caused by another operation never delivers the current Recover
  event. Exact inspection either supersedes it with audit evidence or retries.
- Quarantine applied/replayed/response-lost paths end Redis dead and DB
  internal_error with the same audit operation; different operation conflicts.

### Trusted process and Redis connectivity

- Render deployment config and prove Redis auth material/network attachment is
  present only for trusted Go services, without printing credential values.
- An ephemeral process attached only to `untrusted` cannot resolve or connect to
  Redis and has no Redis environment, secret mount, socket, or inherited fd.
- Assert Redis/PostgreSQL have no published host port and Redis ACL rejects
  administrative commands while making no per-script authorization claim.
- Assert the mock worker depends only on WorkerQueue and cannot compile against
  ApplyTerminalOutbox/Recover/Quarantine/Cleanup through that interface.
- Assert Phase 1 has no submitted-source execution or child-process path.

### Ordered outbox concurrency and restart

- Insert N and N+1, run concurrent dispatchers, and prove only lowest pending N
  reaches Redis; after committed deliver/supersede, exactly one dispatcher may
  dispatch N+1.
- Crash before Redis, after applied Redis but before DB acknowledgement, and
  after DB acknowledgement before client response; restart converges the same
  operation without double transition.
- Backoff on N blocks N+1. Retry/age threshold emits audit decision work but does
  not silently supersede N.
- When another operation won, exact Redis plus durable DB evidence records
  `superseded_by_operation_id`; ambiguous evidence leaves N pending.
- Concurrent deliveries progress independently while same-delivery ordering is
  serialized.

### Type, identity, and terminal tests

- Wrong type for every fixed/dynamic key in every script causes no intended
  mutation.
- Redis 7 TYPE reply behavior is exercised directly.
- No script contains LREM, marker insertion, or payload identity.
- Complete twice, Fail twice, Complete then Fail, Fail then Complete.
- Worker Complete/Fail on requeued state is STATE_CONFLICT. Exact
  TerminalAuthorization applies once; wrong op/event/requeue binding, stale
  worker, and later generation cannot mutate.
- Marker/delimiter-like metadata cannot collide with separate identity fields.

### PostgreSQL/Redis integration

- Source-current Compose test injects DB Start rejection and verifies immediate
  ReleaseClaim despite unexpired lease.
- Inject DB unknown outcomes, Redis failures after terminal/recover/release
  outbox commits, out-of-order dispatch attempts, and reconciler restart.
- Verify queue HTTP 503, queued quota HTTP 409, durable enqueue_unknown polling,
  release/recovery-limit audit and DB convergence, trusted-network isolation,
  ordered outbox restart, terminal API separation, and Cleanup resume.
- No integration test uses a stale application image or skips real Redis/DB.

## 16. Acceptance criteria

- Claim scans all active queued work under the hard 500 bound; position-500
  eligibility test proves no prefix starvation.
- Quotas and active depth derive from queued/processing indexes, never counters.
- Processing is a lease-deadline ZSET; deadline scans are deterministic and
  structural audits use persistent cursors.
- Redis TIME inside Claim/Inspect/RecoveryCandidates/Recover is the sole lease
  authority; worker clock skew cannot change deadlines or stale decisions.
- Phase 1 hard timeout, safety margin, Redis timeout, and lease duration validate
  at startup; work cancellation precedes lease expiry. Phase 2 must re-review
  renewal.
- ReleaseClaim handles DB Start rejection without lease expiry and is tokenized,
  operation-identified, bounded, atomic, and present in ordered outbox protocol.
- Recover and Release replay requires the same operation ID and kind; another
  operation cannot be reported as the winner or falsely complete its outbox.
- Claim retry/InspectAttempt reports every valid later attempt lifecycle.
- Claim IDs are durably reserved before Redis use and cannot be reused after
  Cleanup.
- Every Lua-parsed/incremented number is canonical uint53 with explicit
  pre-mutation exhaustion behavior.
- Recovery moves to tail and deterministically dead-letters at the configured
  recovery limit.
- Idempotent Enqueue first validates full state-specific index invariants.
- PostgreSQL audit schema/outbox and retention rules exist for Quarantine.
- Cleanup has a persistent marker/phase, first/resume modes, exact manifest,
  missing-safe partial retry, foreign-map conflict, and last-step verification.
- Enqueue has durable pending/unknown/rejected/queued DB states, truthful HTTP
  outcomes, retained source on unknown, and idempotent reconciliation.
- Ambiguous attempt-binding corruption is quarantined; no undefined Phase 1
  RepairAttemptBinding remains.
- Worker terminal methods cannot mutate queued/requeued work; only an exact,
  ordered PostgreSQL TerminalAuthorization can converge it.
- Trusted-process/network/credential isolation keeps untrusted code away from
  Redis; Redis ACL is not claimed as per-script worker/dispatcher authorization.
- Recovery-limit and Quarantine converge Redis dead-letter to PostgreSQL
  internal_error using the exact operation/token and idempotent response-loss
  inspection.
- Dispatcher locks a delivery then selects/locks only its lowest unresolved
  event; N+1 cannot run until N is delivered or explicitly audited/superseded.
- Expected errors precede mutation; unexpected runtime/transport failure is
  outcome-unknown and never triggers caller compensation.
- Delivery IDs, attempt/generation CAS, exact ZREM, terminal idempotency, wrong
  type safety, no TTL, and no payload/marker/LREM rules hold.
- Real Redis 7, race, full Go, vet, build, formatting, diff, and source-current
  PostgreSQL/Redis integration gates pass with zero required skips.

## 17. Self-review against historical and external blockers

| Failure/blocker | Concrete resolution | Status |
|---|---|---|
| Bounded Claim prefix starvation | Full ZRANGE of <=500 queued items; lowest eligible ready sequence | covered |
| Participant can block global queue | All entries examined; position-500 test | covered |
| Authoritative counters drift/negative | Counters removed; quota/depth from indexes; telemetry disposable | covered |
| No participant membership index | Bounded O(P+Q) metadata scan explicitly accepted at max 500 | covered |
| SET recovery scan nondeterministic | Processing ZSET score=deadline; ZRANGEBYSCORE ordered batches | covered |
| Structural processing orphans unseen | Persistent state HSCAN plus processing ZSCAN audit epochs | covered |
| DB Start rejection blocked by fresh lease | Separate exact-token ReleaseClaim with no lease-age guard | covered |
| Crashing delivery retains queue head | Recover/Release allocate tail ready sequence | covered |
| Poison delivery retries forever | Recovery count and deterministic dead-letter limit | covered |
| Attempt/tombstone mappings grow forever | PostgreSQL-authorized exact Cleanup removes all record/attempt fields | covered |
| Corrupt existing record returns enqueue success | State-specific invariants precede IDEMPOTENT_EXISTING | covered |
| Quarantine audit has no storage | PostgreSQL audit table, ordered outbox, operation ID, retention | covered |
| Lua error assumed rollback | Runtime/transport errors are outcome-unknown; inspect/reconcile, no compensation | covered |
| Redis 7 TYPE status reply | Safe TYPE normalizer plus real Redis type matrix | covered |
| Payload/marker exact-removal failures | Opaque delivery ZSET member; no payload, marker, list, or LREM | covered |
| Delayed terminal touches new attempt | Opaque non-reused AttemptID plus generation CAS | covered |
| Complete/Fail/Recover replay changes twice | Tokenized stable state results | covered |
| Missing/malformed/inconsistent lease | Recover reasons plus structural cursor audit; ambiguous binding quarantines | covered |
| Wrong Redis key type mutates | Every key type checked before first write | covered |
| Redis/PostgreSQL ordering divergence | Token columns, Release/Recover/terminal outbox, ordered dispatcher | covered |
| Short TTL destroys state | No TTL; coordinated retention cleanup only | covered |
| Redis tests silently skip/stale image | Fail-not-skip real services and source-current Compose gates | covered |
| Recover and Release share attempt-only idempotency | Dedicated requeue operation ID/kind/token/reason/event binding; other operation conflicts | covered |
| Claim retry after a valid later transition | InspectAttempt derives processing/recovered/released/terminal/dead/superseded/cleaned | covered |
| Lua rounds uint64 above 2^53 | Canonical uint53 for all Lua numbers and explicit exhaustion | covered |
| Release/DB Start rejection loops forever | release_count, MAX_RELEASE_ATTEMPTS, audit, deterministic dead-letter; permanent inconsistency quarantines | covered |
| Cleanup runtime failure leaves partial data | persistent marker/phase, immutable manifest, first/resume modes, fault-injection tests | covered |
| Enqueue rejection after DB commit is undefined | pending_enqueue/queued/enqueue_rejected/enqueue_unknown protocol and durable source cleanup | covered |
| Undefined RepairAttemptBinding | removed from Phase 1; ambiguous binding always audited Quarantine | covered |
| Old worker terminalizes a requeued delivery | worker API processing-only; queued terminalization requires exact PG TerminalAuthorization | covered |
| Worker clock skew changes lease/staleness | Claim, Inspect, candidates, and Recover use one Redis TIME per script; caller now removed | covered |
| Redis ACL claimed as per-script capability | Trusted-process/network boundary chosen; ACL only hardens commands/keyspace | covered |
| Recovery limit/Quarantine leaves DB nonterminal | Exact operation/token CAS converges to internal_error and response loss inspects before ack | covered |
| Advisory lock does not select event order | Lock delivery, then SELECT lowest unresolved event ORDER BY event_seq FOR UPDATE | covered |

## 18. Design verdict

**DESIGN READY — FROZEN AFTER REVIEW 04**

The final four blockers have schema/configuration, exact algorithms, failure and
outcome-unknown behavior, idempotent retry, and integration tests. No further
design review is planned. The proposed implementation checkpoint A is limited
to Redis v3 schema plus read-only InspectDelivery/InspectAttempt and Enqueue;
Claim or other lifecycle mutation is outside checkpoint A.
