# HUSTack Multi-Agent Rules

## Roles
- Codex: coordinator, reviewer, security auditor, test owner, Git gatekeeper.
- OpenCode: implementation worker.
- OpenCode never commits. Codex may commit locally after review.
- Neither agent pushes to GitHub.

## Current phase
Phase 1 remediation only. Do not add GCC, submitted-code execution,
sandbox, libhsruntime.so, fd 3, broker, VM, private object, flag, or the
intended verifier/executor vulnerability.

## Credentials
Never read `.env`, `/mnt/d/ctf/.env`, or
`~/.config/hustack/github-token`. Never print environment variables.
Never run `git push`.

## Required documents
Read:
1. HUSTack_Trusted_Runtime_Challenge_Design.md
2. 00_MASTER_PROMPT.md
3. PHASES.md
4. 01_PHASE_1_PROMPT.md
5. docs/phase-1-security-notes.md when present

## Invariants
- No shell execution of user input.
- Parameterized SQL only.
- Escape all untrusted output.
- Enforce source limits by streaming.
- Do not trust arbitrary proxy headers.
- Do not log source or credentials.
- Queue must not lose or duplicate jobs.
- PostgreSQL and Redis must remain reconcilable.
- Counters cannot become negative.
- Complete, Fail, and Recover must be idempotent.

## Handoff
Codex writes `docs/agent_handoff/OPENCODE_TASK_XX.md`.
OpenCode writes `docs/agent_handoff/OPENCODE_REPORT_XX.md`.
Codex writes `docs/agent_handoff/CODEX_REVIEW_XX.md`.
