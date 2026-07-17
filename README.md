# HUSTack — Trusted Runtime

## Phase 1 — Secure web/API foundation

A secure, asynchronous source-submission web application for the HUSTack CTF challenge. This phase implements the web frontend, API, PostgreSQL metadata storage, Redis-backed queue and rate limiting, mock worker, and Nginx reverse proxy.

**Compilation and execution are mocked in this phase.**

## Prerequisites

- Docker & Docker Compose (v2)
- GNU Make (optional, for Makefile targets)

## Quick start

```bash
# Start all services
make dev

# Or manually:
docker compose up --build -d

# Run migrations
make migrate

# View logs
make logs
```

## URLs

| Service  | URL                        |
|----------|----------------------------|
| Website  | http://localhost:8080      |
| API      | http://localhost:8080/api  |
| Nginx    | http://localhost:8080      |

## Configuration

Copy `.env.example` to `.env` and adjust:

```bash
cp .env.example .env
```

Required variables are documented in `.env.example`. In production mode (`APP_ENV=production`), `COOKIE_HMAC_SECRET` must be at least 32 characters.

## Commands

```bash
make dev              # Start all services
make down             # Stop all services
make migrate          # Run database migrations
make fmt              # Format Go source code
make lint             # Run go vet
make test             # Run unit tests (short mode)
make integration-test # Run integration tests
make phase1-acceptance # Rebuild and run the destructive, isolated Phase 1 gate
make logs             # Follow container logs
```

## Architecture

```
Nginx (port 8080)
  └── Go API (:8080)
        ├── PostgreSQL (authoritative lifecycle, quotas, outbox)
        └── Redis (Streams delivery transport + rate limits)
              ⇅
            Mock Worker (outbox dispatch, stream consume, stale recovery)
```

- **Nginx**: Reverse proxy, request body limit, rate limiting, security headers
- **Go API**: Submission handling, validation, identity management, CSRF protection
- **PostgreSQL**: Authoritative `queued`/`mock_processing` lifecycle, global capacity, participant queued/running quotas, and transactional enqueue outbox
- **Redis**: Distributed token buckets and the `hustack:queue:v2` at-least-once Stream transport; consumer group `mock-workers`
- **Mock Worker**: Dispatches committed outbox events with `XADD`, consumes new messages with `XREADGROUP`, reclaims stale pending messages with `XAUTOCLAIM`, and conditionally finishes database rows

Submission creation commits the queued row and enqueue outbox event together. A Redis outage therefore cannot lose an accepted submission. A lost Redis response may produce a duplicate Stream entry; conditional PostgreSQL transitions make duplicates safe. Completed or obsolete deliveries use `XACK` followed by `XDEL`. This is not distributed exactly-once: if `XDEL` fails, an acknowledged record may remain in the Stream until cleanup, but it is no longer pending.

Stale `mock_processing` rows are conditionally returned to `queued` and receive a new outbox event in the same PostgreSQL transaction. Redis lifecycle counters and Go-side Redis/PostgreSQL compensation are not used.

## Upload limits

- Maximum source size: **10 MiB** (10,485,760 bytes)
- Enforced at three layers: Nginx, API streaming reader, storage layer
- Only `.c` files accepted (case-insensitive extension)
- No archives (ZIP, TAR, GZIP)
- NUL bytes rejected
- Empty source rejected

## Rate limits

| Scope           | Limit                             |
|-----------------|-----------------------------------|
| Per participant | 6 submissions/minute              |
| Per participant | 120 submissions/hour              |
| Per IP          | 20 submissions/minute             |
| Polling         | 60 reads/minute per participant   |
| Max queued      | 3 per participant                 |
| Max running     | 1 per participant                 |
| Global queue    | 500 jobs maximum                  |

## Identity

Phase 1 uses cookie-based identity:
- Random participant ID generated on first visit
- Stored in `HttpOnly`, `SameSite=Lax`, HMAC-signed cookie
- No login/password system yet

## Testing

```bash
make test              # Unit tests (no external dependencies needed for most)
make integration-test  # Integration tests (requires running Docker environment)
make phase1-acceptance # Full clean rebuild, real dependency, and isolated acceptance gate
```

## Security notes

See `docs/phase-1-security-notes.md` for detailed security documentation.

## Current limitations (Phase 1)

- No actual GCC compilation
- No sandbox execution
- No runtime library (`libhsruntime.so`)
- No broker protocol
- No bytecode VM
- All results are mocked
