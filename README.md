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
make logs             # Follow container logs
```

## Architecture

```
Nginx (port 8080)
  └── Go API (:8080)
        ├── PostgreSQL (metadata)
        └── Redis (queue + rate limit)
              └── Mock Worker (async job processing)
```

- **Nginx**: Reverse proxy, request body limit, rate limiting, security headers
- **Go API**: Submission handling, validation, identity management, CSRF protection
- **PostgreSQL**: Submission metadata and participant records
- **Redis**: Bounded job queue and distributed token-bucket rate limiter
- **Mock Worker**: Consumes queued jobs, simulates processing (200ms delay), writes mock results

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
