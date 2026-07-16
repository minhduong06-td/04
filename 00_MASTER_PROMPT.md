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
