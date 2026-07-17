# Phase 1 Security Notes

## Upload handling

- **10 MiB hard cap**: Enforced at Nginx (`client_max_body_size 11m` to account for multipart overhead), API (`http.MaxBytesReader` on request body), and storage layer (`io.LimitReader` streaming)
- **Streaming enforcement**: Uses `r.MultipartReader()` for streaming reads; body parsed part-by-part via `mr.NextPart()` without buffering the entire body
- **Streaming pipeline per part**: `stripBOM` → `storage.SaveStream` (NUL detection, size limit, SHA-256, temp file)
- **No `strings.Builder` for source_text**: source_text is streamed through the same pipeline as source_file
- **Extension validation**: Only `.c` files accepted, case-insensitive check on filename suffix
- **NUL byte rejection**: Source scanned during streaming write; rejected if any NUL byte found
- **Empty source rejection**: Zero-byte source is rejected at storage layer
- **No archives**: ZIP, TAR, GZIP not supported; only raw text or single `.c` file
- **UTF-8 BOM**: Only exact three-byte BOM (`EF BB BF`) stripped; single `EF` preserved; no partial-byte removal
- **Temp file cleanup**: If bad part follows a valid source part, the temp file is deleted before returning error

## Rate limiting

- **Redis-backed token bucket**: Atomic Lua script for token refill and consumption
- **Token bucket with creation timestamp**: First access records `:ts` key; refill computed from elapsed intervals since last refill; timestamp advanced by used intervals only
- **Injectable clock**: Tests use `SetClock()` for deterministic time without long sleeps
- **Rate validation**: `rate <= 0` is treated as unlimited (returns immediately)
- **Per participant**: 6/minute + 120/hour using separate token buckets
- **Per IP**: 20/minute via `X-Forwarded-For` or `RemoteAddr` (from trusted proxy or direct)
- **Polling**: 60/minute per participant for result polling
- **Queue quotas**: PostgreSQL is authoritative. Submission transactions enforce max 3 queued per participant and a global active (`queued` plus `mock_processing`) limit of 500; claim transactions enforce max 1 `mock_processing` per participant.
- **Global capacity response**: HTTP 503 with `Retry-After`; participant queued quota returns HTTP 409.
- **Response codes**: 429 (rate limit), 409 (quota), 413 (body/source too large), 503 (queue full), with `Retry-After` header

## Queue backpressure and delivery

- PostgreSQL is the lifecycle and quota source of truth for queued and `mock_processing` rows, global active capacity, and participant queued/running quotas.
- Submission creation takes transaction-scoped advisory locks, enforces capacity/queued quota, and commits the submission plus an `enqueue_submission` outbox event atomically. A rejected transaction creates neither row.
- Redis Stream `hustack:queue:v2`, consumer group `mock-workers`, is transport only. Outbox delivery uses `XADD`; workers use `XREADGROUP`; stale pending delivery uses `XAUTOCLAIM`.
- Redis outage after database acceptance cannot lose a submission because the outbox remains retryable. Response loss can duplicate `XADD`; duplicate transport delivery is expected and safe because PostgreSQL starts and finishes rows with conditional transitions.
- Workers acknowledge obsolete/completed entries with `XACK`, then trim that exact record with `XDEL`. These two commands are not distributed exactly-once: an `XDEL` failure can leave an acknowledged record until later cleanup, but cannot make it pending again.
- Stale `mock_processing` recovery is one PostgreSQL transaction: conditionally return the row to `queued` and insert a new enqueue outbox event. It does not use Redis lifecycle counters or a Go-side compensating Redis move.

## CSRF

- HMAC-signed CSRF token generated per participant session
- Token embedded in HTML form as hidden input for convenience
- **Verified on `X-CSRF-Token` header only** (NOT from form value)
- Token includes timestamp with 2-hour expiry
- Origin enforced via `VerifyOrigin()` on all POST requests; rejects mismatched `Origin` header

## Participant identity

- Random UUIDv4-like participant ID generated on first visit
- HMAC-SHA256 signed cookie (`HttpOnly`, `SameSite=Lax`, `Secure` in production)
- Cookie format: `base64(participant_id:issued_at:expires_at)` signed with HMAC-SHA256
- Secret from environment variable; minimum 32 bytes in production
- Tampered cookies are rejected (signature mismatch)
- Interface-based design allows future provider swap (e.g., trusted proxy header)
- No passwords stored in Phase 1
- `X-Team-ID` header from client is never trusted

## SQL injection prevention

- All queries use parameterized placeholders (`$1`, `$2`, etc.)
- No string concatenation in SQL
- Opaque UUID submission IDs prevent sequential IDOR

## IDOR prevention

- Every submission read query includes `participant_id` in WHERE clause
- Non-owned submissions return HTTP 404 (not 403) to avoid confirming existence
- Submission IDs are non-sequential UUIDs

## XSS prevention

- Go `html/template` auto-escapes all template values
- JavaScript uses `textContent` / `innerText` — never `innerHTML`
- CSP header restricts script execution to same-origin only
- No `template.HTML` usage
- Output from submission (stdout, stderr) rendered as escaped text
- Original filenames sanitized (path separators replaced, length truncated)

## Source storage

- Stored outside public web root at `/var/lib/hustack/sources/`
- Filename is opaque UUID generated server-side
- Client filename never used as filesystem path
- Written atomically (temp file via `SaveStream` then `Commit` rename)
- Permissions set to 0640
- SHA-256 computed during streaming write
- No follow symlinks (path traversal detection in `safePath`)
- No overwrite of existing source keys
- Not stored in application logs

## Client IP trust

- Nginx sets `X-Forwarded-For $remote_addr` and `X-Real-IP $remote_addr`, overwriting any client-supplied value
- API only trusts proxy headers when request comes from `TRUSTED_PROXY_CIDR`
- Current Compose `TRUSTED_PROXY_CIDR` default: `172.18.0.0/16`
- Untrusted clients cannot spoof their IP via headers

## Logging

- Structured JSON logging via `log/slog`
- Logged: request ID, participant ID (first 8 chars), submission ID, source size, SHA-256, status transitions, queue depth, rate-limit decisions
- Never logged: source contents, stdout/stderr, cookie values, CSRF tokens, database/Redis passwords, HMAC secret
- Original filenames sanitized before logging

## Deferred to later phases

- GCC compilation and execution sandbox
- `libhsruntime.so` runtime library
- fd 3 and Unix socket broker protocol
- Bytecode VM and intended 16/32-bit vulnerability
- Private object and flag retrieval
- Production CSP tuning for broker-specific features
- Per-team flag injection via broker
- Separate least-privilege database users (currently using owner-only in Compose)
