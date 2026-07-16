# HUSTack Trusted Runtime — Master Build Prompt

You are the lead implementation agent for a security CTF challenge named **HUSTack — Trusted Runtime**.

The authoritative design specification is the attached document:

- `HUSTack_Trusted_Runtime_Challenge_Design.md`

Read it fully before making changes. When the repository contains a copy, keep it under:

```text
docs/HUSTack_Trusted_Runtime_Challenge_Design.md
```

## Mission

Build a production-oriented blackbox CTF challenge where participants submit valid GNU C17 source code. The intended solution chain is:

```text
submit valid C source
→ inspect /proc/self/maps and /proc/self/fd
→ discover and dump an internal runtime shared library
→ reverse the runtime and its custom broker protocol
→ identify a verifier/executor integer-width mismatch
→ submit bytecode accepted as a public object by the verifier
→ executor resolves it as a private answer object
→ retrieve the flag
```

The challenge must not depend on:

- ELF upload tricks or magic-byte manipulation;
- path traversal;
- SQL injection;
- command injection;
- archive extraction;
- kernel exploitation;
- a real container/host escape;
- timing-sensitive races;
- unintended memory-corruption bugs.

## Required implementation stack

Use a simple, auditable stack:

- Go, current stable release, for API, queue worker, runner orchestration, and broker;
- standard `net/http` or a small router; avoid large web frameworks;
- PostgreSQL for submission metadata;
- Redis for queueing and distributed rate limits;
- server-rendered HTML templates with minimal vanilla JavaScript;
- Nginx as reverse proxy;
- Docker Compose for local development;
- OCI containers with rootless execution where practical.

Do not add React, Next.js, a Node build chain, Kubernetes, or other unnecessary infrastructure.

## Global engineering rules

1. **Never execute user input through a shell.**
   - No `system()`.
   - No `sh -c`.
   - No `shell=True`.
   - Compiler and runner arguments must be fixed argv arrays.

2. **Treat source, filename, stdout, stderr, and compiler output as untrusted.**
   - Render with escaped text only.
   - Never inject them into HTML.

3. **Upload constraints must be enforced at three layers.**
   - Nginx.
   - API streaming reader.
   - Worker before compilation.
   - Hard limit: `10 MiB = 10,485,760 bytes`.
   - Reject NUL bytes.
   - Accept exactly one source: `source_text` or `source_file`.
   - Only a single `.c` file; no archives or URLs.

4. **Use opaque submission IDs.**
   - UUIDv7, UUIDv4, or ULID with sufficient entropy.
   - Never expose sequential database IDs.
   - Every read must check submission ownership.

5. **Rate limiting is mandatory.**
   - Per participant/account.
   - Per IP.
   - Bounded global queue.
   - Maximum one running submission per participant.
   - Maximum three queued submissions per participant.
   - Do not hold HTTP requests open while compilation or execution runs.

6. **Resource limits are mandatory.**
   - Compile timeout: 8 seconds.
   - Runtime CPU: 3 seconds.
   - Runtime wall time: 4 seconds.
   - Runtime memory: 128 MiB.
   - PIDs: 32.
   - Output: 64 KiB total.
   - Kill the complete process tree/cgroup on timeout or output overflow.

7. **Isolation boundaries must be real.**
   - Web API never compiles or runs user programs itself.
   - Compiler and runner are separate services.
   - Broker is a separate service/container.
   - Submission containers have no external network.
   - Do not mount Docker socket, host root, host `/proc`, secrets, SSH keys, or database credentials.

8. **The broker must accept object IDs only.**
   - No user-controlled paths.
   - No command execution.
   - No arbitrary file API.
   - Only fixed object allowlists.
   - The intended verifier/executor mismatch must be the only designed privilege bypass.

9. **Database access must be parameterized.**
   - No string-built SQL.
   - Use least-privilege DB users.
   - Do not log flags or full source by default.

10. **Do not prematurely implement later phases.**
    - Complete one phase.
    - Run its tests.
    - Produce a phase report.
    - Stop and wait for approval.

## Repository quality requirements

Every phase must preserve:

```text
make fmt
make lint
make test
make integration-test
```

Add or update:

- `README.md`
- `.env.example`
- `Makefile`
- migrations
- unit tests
- integration tests
- security notes
- architecture decision records when a decision is non-obvious

Do not leave placeholder TODOs for security-critical behavior.

## Required phase report

At the end of every phase, return:

1. Summary of implemented work.
2. Exact files created/changed.
3. Commands to run locally.
4. Test results.
5. Known limitations.
6. Security assumptions.
7. Decisions required before the next phase.

Stop after the requested phase.


---

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


---

# Copy-paste Prompt for Coding Agent — Phase 1

You are implementing **Phase 1: Secure web/API foundation** for the HUSTack Trusted Runtime challenge.

Read these repository documents first:

```text
docs/HUSTack_Trusted_Runtime_Challenge_Design.md
docs/PHASES.md
```

If they are not present, copy the supplied specification and phase plan into `docs/` before coding.

## Phase objective

Create a secure, asynchronous source-submission web application. This phase must **not compile or execute user code**. A mock worker will consume queued jobs and transition them to a completed mock result.

Do not implement the internal runtime, fd 3, broker protocol, VM, private object, or intended vulnerability yet.

## Required stack

- Go for API and mock worker.
- PostgreSQL for metadata.
- Redis for job queue and distributed token-bucket limits.
- Nginx reverse proxy.
- Server-rendered HTML templates plus minimal vanilla JavaScript.
- Docker Compose for local development.

Prefer the Go standard library and small focused packages. Avoid a large framework.

## Repository structure

Create or adapt toward:

```text
.
├── cmd/
│   ├── api/
│   │   └── main.go
│   └── mock-worker/
│       └── main.go
├── internal/
│   ├── config/
│   ├── database/
│   ├── identity/
│   ├── queue/
│   ├── ratelimit/
│   ├── submissions/
│   └── web/
├── migrations/
├── web/
│   ├── templates/
│   └── static/
├── deploy/
│   └── nginx/
├── docs/
├── tests/
├── docker-compose.yml
├── Makefile
├── .env.example
└── README.md
```

Use the existing repository layout where reasonable rather than moving files gratuitously.

## Identity model

Implement a pluggable participant identity interface.

For development:

- If no authenticated identity exists, issue a signed, `HttpOnly`, `SameSite=Lax` participant cookie.
- The cookie contains only an opaque random participant ID and expiry.
- Sign it with an HMAC secret from environment.
- Do not trust an arbitrary public `X-Team-ID` header.

Prepare an interface so production can later resolve identity from a trusted reverse-proxy header, but keep that mode disabled unless explicitly configured.

Store no passwords in this phase.

## Submission data model

Create a migration with at least:

```text
participants
- id: opaque UUID/ULID primary key
- created_at
- last_seen_at

submissions
- id: opaque UUID/ULID primary key
- participant_id foreign key
- original_filename nullable
- source_storage_key
- source_size
- source_sha256
- status
- compile_success nullable
- compile_stderr nullable
- exit_code nullable
- stdout nullable
- stderr nullable
- result_truncated boolean
- created_at
- updated_at
- started_at nullable
- finished_at nullable
```

Allowed states:

```text
queued
mock_processing
finished
internal_error
```

Do not use sequential public IDs.

All reads must include participant ownership checks.

Use parameterized SQL only.

## Source storage

Do not store source in logs.

Store source outside the public web root using a generated storage key:

```text
/var/lib/hustack/sources/<opaque-id>.c
```

Requirements:

- Ignore the client filename for the filesystem path.
- Create files with restrictive permissions.
- Use atomic write then rename.
- Reject symlinks and path reuse.
- Compute SHA-256 while streaming.
- Store the original filename only as escaped metadata.
- Set a configurable retention marker for later cleanup.

A local filesystem implementation is sufficient for Phase 1, behind a small storage interface.

## Submission endpoint

Implement:

```http
POST /api/submissions
```

Accept either:

- form field `source_text`; or
- multipart file `source_file`.

Exactly one must be present.

Validation:

- Hard maximum `10,485,760` bytes.
- Enforce by streaming; do not rely only on `Content-Length`.
- Reject NUL bytes.
- Uploaded filename must end in `.c`, case-insensitive.
- Do not accept ZIP, TAR, GZIP, URLs, multiple files, or compiler flags.
- Do not perform unsafe Unicode normalization.
- UTF-8 BOM may be removed only when the exact first three bytes are `EF BB BF`.
- Normalize CRLF to LF only if implemented consistently and covered by tests.
- Empty source is rejected.
- Source over 1 MiB should emit a structured security/usage metric but remain allowed up to 10 MiB.

On success:

1. resolve participant;
2. check participant and IP rate limits;
3. enforce queue/concurrency quotas;
4. save the source;
5. insert a `queued` submission;
6. enqueue the opaque submission ID;
7. return HTTP 202 with JSON containing the ID and status.

Do not keep the request open for job completion.

## Result endpoints

Implement:

```http
GET /api/submissions/{id}
GET /submissions/{id}
```

Both must verify ownership.

The JSON endpoint returns current state and mock result.

The HTML endpoint renders a submission page and polls the JSON endpoint with bounded frequency.

Do not render any untrusted field with `template.HTML` or `innerHTML`. Use escaped templates and `textContent`.

## Frontend

Create a minimal page:

- challenge title and short description;
- textarea for source C;
- `.c` file chooser;
- clear display of 10 MiB maximum;
- submit button;
- validation error area;
- result page with status, stdout, stderr, and truncation indicator.

No client-side framework.

Client-side checks are UX only; server validation remains authoritative.

## Queue and mock worker

Use Redis for a bounded queue.

The mock worker:

1. claims a queued submission;
2. changes status to `mock_processing`;
3. waits a short configurable duration such as 200 ms;
4. writes a deterministic mock result:
   - `compile_success=true`
   - `exit_code=0`
   - `stdout="Phase 1 mock runner: source accepted\n"`
   - empty stderr
5. sets status `finished`.

Requirements:

- Idempotent claim/update logic.
- At-least-once delivery safe.
- Stale job recovery strategy documented.
- Queue length hard limit, default 500.
- When full, API returns HTTP 503 and `Retry-After`.

Do not load the source into Redis.

## Rate limits and quotas

Implement Redis-backed token buckets with configurable defaults:

```text
per participant:
- 6 submissions/minute
- 120 submissions/hour

per IP:
- 20 submissions/minute

per participant:
- max 1 mock_processing
- max 3 queued
```

Polling:

```text
60 result reads/minute per participant
```

Return:

- HTTP 429 for participant/IP rate limit;
- HTTP 409 for participant queue/concurrency quota;
- HTTP 503 for global queue saturation.

Include `Retry-After` where meaningful.

Do not rely only on Nginx for application quotas.

## Nginx

Add development/production-oriented config with:

```text
client_max_body_size 10m
client_body_timeout 10s
client_header_timeout 10s
send_timeout 15s
keepalive_timeout 15s
connection limit per IP
request rate limit for POST /api/submissions
less strict rate limit for polling
security headers
```

Do not expose PostgreSQL or Redis through Nginx.

## Security headers

At minimum:

```text
Content-Security-Policy
X-Content-Type-Options: nosniff
Referrer-Policy
Permissions-Policy
frame-ancestors or X-Frame-Options
```

Use a CSP compatible with server-rendered pages and external-script-free vanilla JavaScript.

## CSRF

Because development identity uses cookies:

- Add CSRF protection for submission POST.
- Use a per-session or signed CSRF token.
- Check `Origin` when available.
- Use `SameSite=Lax`.

The JSON polling GET does not modify state.

## Logging

Use structured logs.

Log:

- request ID;
- participant ID in a non-sensitive opaque form;
- submission ID;
- source size;
- source hash;
- status transitions;
- rate-limit decisions;
- queue depth.

Do not log:

- source contents;
- stdout/stderr contents;
- cookie values;
- CSRF token;
- secrets.

Escape or structurally encode original filenames to prevent log injection.

## Configuration

Use environment variables and provide `.env.example`.

Include:

```text
HTTP_ADDR
DATABASE_URL
REDIS_ADDR
SOURCE_STORAGE_ROOT
COOKIE_HMAC_SECRET
PUBLIC_BASE_URL
MAX_SOURCE_BYTES=10485760
QUEUE_MAX_DEPTH=500
SUBMIT_RATE_PER_MINUTE=6
SUBMIT_RATE_PER_HOUR=120
IP_SUBMIT_RATE_PER_MINUTE=20
POLL_RATE_PER_MINUTE=60
MAX_QUEUED_PER_PARTICIPANT=3
MAX_RUNNING_PER_PARTICIPANT=1
MOCK_WORK_DURATION
```

Fail startup when required secrets are absent or insecure defaults are used outside development mode.

## Docker Compose

Provide services:

```text
nginx
api
mock-worker
postgres
redis
```

Requirements:

- health checks;
- service dependencies based on health;
- Redis/PostgreSQL not published publicly by default;
- named volumes for database and source storage;
- API and worker run as non-root;
- read-only filesystems where practical;
- tmpfs for temporary paths;
- no privileged mode;
- no Docker socket mounts.

## Tests

Add unit tests for:

- exactly one source input;
- empty source;
- 10 MiB accepted;
- 10 MiB + 1 byte rejected;
- chunked/streaming over-limit rejection;
- NUL byte rejection;
- exact UTF-8 BOM handling;
- `.c` extension check;
- traversal-like original filename stored safely;
- signed participant cookie;
- CSRF validation;
- token bucket behavior;
- queue quota;
- ownership checks;
- HTML escaping of source-related output.

Add integration tests for:

1. submit textarea source;
2. submit `.c` file;
3. receive queued state;
4. mock worker completes;
5. owner can read result;
6. another participant receives 404 or 403 without confirming existence;
7. rate-limited participant receives 429;
8. queue saturation receives 503;
9. oversized upload receives 413;
10. stdout containing `<script>` is displayed as text, not executed.

## Make targets

Provide:

```text
make dev
make down
make migrate
make fmt
make lint
make test
make integration-test
make logs
```

`make dev` should bring up the complete Phase 1 environment.

## Documentation

Update `README.md` with:

- prerequisites;
- startup commands;
- URLs;
- environment configuration;
- test commands;
- Phase 1 architecture;
- explicit statement that compilation/execution is mocked.

Add:

```text
docs/phase-1-security-notes.md
```

Document:

- upload limits;
- rate-limit model;
- queue backpressure;
- CSRF;
- ownership/IDOR protection;
- storage model;
- known limitations deferred to later phases.

## Acceptance criteria

Phase 1 is complete only when:

- `docker compose up --build` starts successfully;
- health checks pass;
- a browser can submit C source;
- the API returns immediately with an opaque submission ID;
- the mock worker completes the job asynchronously;
- result polling works;
- all upload limits are enforced server-side;
- application and Nginx rate limits work;
- ownership checks prevent cross-participant reads;
- untrusted output is escaped;
- PostgreSQL and Redis are not exposed publicly;
- all tests pass;
- no code compilation or execution exists anywhere in the Phase 1 implementation.

## Required final response

At completion, report:

1. architecture summary;
2. files added or changed;
3. exact commands used;
4. unit and integration test results;
5. unresolved issues;
6. security assumptions;
7. questions or decisions needed before Phase 2.

Stop after Phase 1. Do not begin the compiler worker.
