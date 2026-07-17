# Queue State Machine Design Review 02

Reviewed document: `docs/QUEUE_STATE_MACHINE_DESIGN.md`

Scope: design review only. No production code or Lua implementation was written.

## Review method

The revised design was checked end-to-end rather than as an appendix. For every
external blocker, this review requires all four of:

1. an authoritative schema representation;
2. an atomic transition or deterministic read/reconciliation path;
3. explicit validation and mutation order;
4. a concrete real-Redis or PostgreSQL/Redis integration test.

## 1. No Claim starvation — PASS

Claim no longer accepts a prefix scan limit. It reads the entire queued ZSET
with `ZRANGE queued 0 -1 WITHSCORES`, under the invariant that queued plus
processing membership is at most `QUEUE_MAX_DEPTH=500`.

It first builds running participant counts from every processing member, then
examines every queued member in ascending `ready_seq` until it finds the oldest
eligible delivery. An eligible member at position 500 is therefore observable
even when positions 1–499 belong to quota-blocked participants.

Required proof: the test plan explicitly fills the first 499 positions with
blocked work and places an eligible delivery at position 500.

## 2. Quota source of truth — PASS

`queued_count` and `running_count` were removed from Redis schema and from every
transition. Quotas derive only from:

- queued ZSET membership plus the participant metadata HASH;
- processing ZSET membership plus the participant metadata HASH.

Enqueue scans queued membership for queued quota. Claim scans processing once
to build `running_by_participant`. Complete, Fail, Recover, ReleaseClaim,
Quarantine, and Cleanup update exact indexes and do not maintain counters.

Any metrics cache is expressly non-authoritative, disposable, and rebuildable.
The old negative-counter lifecycle invariant and counter error code were
removed. Tests corrupt/omit telemetry and prove quota decisions are unchanged.

## 3. Deterministic recovery scanning — PASS

Processing is now a ZSET:

```text
member = delivery_id
score  = lease_deadline_ms
```

Normal candidate discovery uses deadline order:

```text
ZRANGEBYSCORE processing -inf now WITHSCORES
```

The full expired set is bounded by the same active maximum of 500. Consequently
a corrupt earliest member cannot pin a bounded prefix and hide later expired
work. Equal-score order is stable by Redis member ordering.

Structural inconsistencies that a deadline query cannot see are covered by two
cursor-preserving reconciliation epochs:

- HSCAN state to find processing records missing processing membership or token
  metadata;
- ZSCAN processing to find members missing state/participant/submission data.

Cursors are stored in PostgreSQL and continue to zero rather than restarting
each tick. Wrong key type stops the epoch without mutation. Exact DB token
evidence permits repair/recovery; ambiguous evidence creates a quarantine audit
instead of blind removal.

## 4. DB Start rejection release path — PASS

The design adds atomic `ReleaseClaim(token, reason)`. It:

- requires the exact active attempt ID and generation;
- does not require lease expiration;
- adds the delivery to queued tail before removing processing membership;
- clears only the exact active/lease binding;
- is idempotent for the same token;
- returns stale after a newer generation.

PostgreSQL Start zero-row or queried-queued outcomes create an ordered
ReleaseClaim outbox event. Recover remains limited to expired, missing,
malformed, or inconsistent lease cases. Integration tests require release of a
fresh unexpired lease after DB Start rejection.

## 5. Poison delivery handling — PASS

The schema contains `attempt_count` and `recovery_count`.
`MAX_RECOVERY_ATTEMPTS` defaults to 3.

Each successful lease recovery increments recovery count exactly once and
allocates a new tail `ready_seq`. At the limit, the same Recover script performs
an atomic dead-letter transition instead of requeue. ReleaseClaim does not
increment recovery count because a rejected DB Start does not prove a poison
worker execution.

Tests cover first/second tail requeue, limit dead-letter, retry idempotency, and
continued progress of other eligible jobs.

## 6. Retention cleanup — PASS

The design adds a PostgreSQL-authorized Cleanup transition. Dispatch is allowed
only after DB terminal state, delivery-ordered outbox completion, elapsed audit
retention, no active attempt, and no queued/processing membership.

Lua revalidates Redis-observable conditions and removes:

- every reverse mapping listed by `:attempts:<delivery_id>`;
- the per-delivery attempts SET;
- dead membership;
- all delivery metadata, state, sequences, generations, attempt/recovery counts,
  attempt fields, quarantine field, and cleanup operation field.

Retry against a fully absent record returns `ALREADY_CLEAN`. Partial or unknown
outcomes are inspected and retried from the durable cleanup outbox. Tests cover
every precondition, exact field removal, reverse-map cleanup, and replay.

## 7. Corrupt idempotent Enqueue behavior — PASS

Before `IDEMPOTENT_EXISTING`, Enqueue validates immutable metadata and the full
state-specific index/token invariants. A queued state without exact queued
membership/score, a processing state without token/lease membership, a terminal
state without terminal metadata, or cross-index duplication returns
`CORRUPT_RECORD` before mutation.

The test plan enumerates each corrupt existing-state case.

## 8. Quarantine audit storage — PASS

Audit storage is explicitly PostgreSQL:

```text
queue_audit_events(operation_id primary key, delivery identity,
                   observed state, reason, evidence, retain_until,
                   redis_applied_at)
```

The audit row and Quarantine outbox event commit together. Redis records the
operation ID in `:quarantine_op`; same-ID replay is idempotent and a different ID
conflicts. Audit retention blocks Cleanup until the audit window expires.

## 9. Outcome-unknown handling — PASS

The common contract distinguishes expected validation results from unexpected
failures:

- all expected errors are selected before the first write;
- after mutation begins, scripts contain no expected error return;
- transport timeout and unexpected script/runtime failure are both
  outcome-unknown because Lua does not roll back earlier commands after a
  runtime command error;
- callers never issue compensating ZREM/ZADD/HDEL operations;
- callers retry the same operation/token or use `InspectDelivery`;
- reconciliation compares actual Redis indexes/state with PostgreSQL delivery,
  attempt, generation, and ordered outbox events.

Fault tests drop replies and inject runtime/transport failures after transition
phases, then require convergence without compensating removal.

## 10. Compatibility with QueueMaxDepth=500 — PASS

The repository default is confirmed as 500. The revised design treats it as the
bound on queued plus processing work, making full Enqueue/Claim scans bounded.
Worst-case complexity is O(P+Q) with P+Q <= 500.

The design requires configuration rejection above 500 unless a future design
adds authoritative participant indexes and is reviewed again. Tests cover item
500, item 501 rejection, and worst-case Lua performance.

## 11. Compatibility with PostgreSQL outbox protocol — PASS

The PostgreSQL protocol now includes:

- delivery/attempt/generation columns;
- monotonic per-delivery outbox event sequence;
- ReleaseClaim event for definitive/queried DB Start rejection;
- Recover events only for lease recovery;
- terminal, Quarantine, and Cleanup events;
- per-delivery advisory locking and ordered/superseded dispatch;
- persistent reconciliation cursors.

The remaining cross-store windows are explicitly non-atomic. Token CAS,
ordered events, inspection, and idempotent retries provide eventual convergence
without claiming a distributed transaction.

## Cross-document consistency checks

- No bounded Claim prefix remains.
- No authoritative queued/running counter key remains.
- No processing SET or repeated-from-zero SSCAN remains.
- No DB Start rollback path uses Recover on a fresh lease.
- No recovery preserves head position indefinitely.
- No unbounded terminal/attempt retention is accepted.
- No corrupt existing record can return idempotent Enqueue success.
- Quarantine has durable, retained audit storage.
- Unexpected Lua errors are not described as rollback-safe.
- No production implementation, Cycle 06 work, or OpenCode task is included.

## Verdict

**DESIGN READY**

This is the historical Review 02 verdict for its stated blocker set. External
Review 03 later identified additional blockers, so this older verdict is not by
itself evidence that the current design is ready.
