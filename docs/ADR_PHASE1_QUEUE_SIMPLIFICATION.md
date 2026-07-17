# ADR: Phase 1 Queue Simplification

Status: accepted and frozen for Phase 1.

## Decision

1. PostgreSQL is authoritative for submission lifecycle and queued/running
   quota state.
2. Redis Streams is an at-least-once delivery transport.
3. Duplicate Redis delivery is expected and safe.
4. PostgreSQL conditional transitions provide lifecycle idempotency.
5. A PostgreSQL transactional outbox guarantees eventual enqueue after a
   submission transaction commits.
6. Redis consumer-group pending entries and `XAUTOCLAIM` provide worker-crash
   transport recovery.
7. The custom Redis v3 state-machine design is retained in this repository as
   historical reference, but is superseded for Phase 1 because its complexity
   exceeds Phase 1 requirements.
8. Phase 1 does not claim distributed exactly-once semantics. It provides
   durable database state, at-least-once transport, and idempotent consumers.

## Consequences

- Redis messages contain only `submission_id` and `participant_id`; they are
  transport records, not lifecycle records.
- Redis counters, list payload identity, `LREM`, temporary markers, and custom
  exactly-once Lua lifecycle machinery are not used by the Phase 1 queue.
- A Redis response loss can produce duplicate stream entries. Database
  conditional transitions make those duplicates harmless and consumers
  acknowledge/delete obsolete deliveries.
- Stale database processing recovery changes `mock_processing` back to `queued`
  and creates a fresh enqueue outbox event in the same PostgreSQL transaction.
