# HUSTack Trusted Runtime — Phased Implementation Plan

## Phase 0 — Repository discovery and architecture lock

Goal:

- Inspect the existing repository.
- Copy the design specification into `docs/`.
- Confirm the stack and service boundaries.
- Write ADRs for identity, queue, source storage, and sandbox technology.
- Produce a dependency and threat-model checklist.
- Make no functional challenge implementation yet.

Deliverables:

- `docs/architecture.md`
- `docs/threat-model.md`
- `docs/adr/*.md`
- updated root `README.md`

---

## Phase 1 — Secure web/API foundation

Goal:

- Build the standalone website and asynchronous submission lifecycle.
- Enforce upload/body limits and rate limiting from the beginning.
- Store submissions safely.
- Use a mock worker only; do not compile or run user code yet.

Deliverables:

- Nginx reverse proxy.
- Go API.
- PostgreSQL migrations.
- Redis queue and rate limiter.
- Minimal server-rendered frontend.
- Mock worker transitioning jobs through states.
- Ownership/IDOR protections.
- Unit and integration tests.
- Docker Compose development environment.

---

## Phase 2 — Compiler worker

Goal:

- Add an isolated compiler worker.
- Compile valid GNU C17 without invoking a shell.
- Apply compile CPU, memory, filesystem, and output limits.
- Return escaped compiler diagnostics.
- Keep runtime execution mocked.

Deliverables:

- compiler container/image;
- fixed compiler argv;
- compile job/result model;
- compile timeout and cleanup;
- compile-bomb tests.

---

## Phase 3 — Hardened submission runner

Goal:

- Execute compiled programs in ephemeral isolated sandboxes.
- Implement namespaces/cgroups/seccomp or the chosen isolation backend.
- Enforce CPU, wall, memory, PIDs, filesystem, and output limits.
- Disable external networking.
- Kill the complete process tree on termination.

Deliverables:

- runner service;
- sandbox policy;
- output streaming limiter;
- timeout/reaper logic;
- fork bomb, infinite output, disk fill, and network denial tests.

---

## Phase 4 — Internal runtime discovery surface

Goal:

- Build `libhsruntime.so`.
- Inject it into submissions.
- Pass a connected Unix `SOCK_SEQPACKET` descriptor as fd 3.
- Ensure `/proc/self/maps` and `/proc/self/fd` expose intended clues only.
- Do not add the vulnerable VM yet.

Deliverables:

- runtime shared object;
- launcher integration;
- broker session stub;
- reproducible internal-library dumping test.

---

## Phase 5 — Authenticated broker protocol

Goal:

- Build the separate `judged` broker.
- Implement hello, session ID, nonce, sequence, request/response frames, authentication, quotas, and public objects.
- Keep all operations memory-safe.
- Expose no user-controlled path or command.
- Still no private-object bypass.

Deliverables:

- broker service;
- runtime client implementation;
- protocol documentation stored privately for authors;
- parser fuzz tests;
- session and quota tests.

---

## Phase 6 — Bytecode VM and intended vulnerability

Goal:

- Implement the small bytecode VM.
- Add independent verifier and executor.
- Deliberately use 16-bit verifier values and 32-bit executor values.
- Add private object `0x00010007`.
- Confirm only the crafted arithmetic expression reaches it.

Deliverables:

- verifier;
- executor;
- public and private object tables;
- intended exploit test;
- negative tests for unintended object reads;
- source-level comments marking the intentionally vulnerable lines.

---

## Phase 7 — Challenge balancing and blackbox polish

Goal:

- Strip symbols from the runtime release build.
- Keep enough observable error codes and clues.
- Add hints and challenge text.
- Tune binary size, output chunking, object offsets, and request limits.
- Write an internal solver.

Deliverables:

- release runtime;
- author solver;
- challenge statement;
- hint schedule;
- difficulty review.

---

## Phase 8 — Security hardening and abuse testing

Goal:

- Audit web, API, database, worker, runner, and broker.
- Verify SQLi, XSS, CSRF, IDOR, SSRF, command injection, archive, oversized-body, Slowloris, compile bomb, fork bomb, disk fill, and output flood protections.
- Tune rate limits and queue backpressure.

Deliverables:

- security test suite;
- abuse test scripts;
- production Nginx configuration;
- monitoring and alerting dashboards/configuration;
- remediation report.

---

## Phase 9 — Deployment and release validation

Goal:

- Produce reproducible production images.
- Add per-team or deployment flag injection.
- Confirm the flag is absent from public images and logs.
- Perform a clean-room solve.
- Document operations, backup, cleanup, and incident response.

Deliverables:

- production Compose/deployment manifests;
- release checklist;
- clean-room solve report;
- operator runbook;
- final challenge package.
