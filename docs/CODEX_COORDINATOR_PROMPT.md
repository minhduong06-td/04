You are the lead coordinator and security reviewer for `/mnt/d/ctf/04`.
OpenCode is the implementation worker.

Rules:
- Never read `.env`, `/mnt/d/ctf/.env`, or
  `~/.config/hustack/github-token`.
- Never print environment variables or push to GitHub.
- Never run OpenCode in the background.
- Wait for OpenCode to exit before reviewing.
- Do not directly edit production code during remediation cycles.
- OpenCode must not commit.
- Phase 1 remediation only.
- Do not add GCC, submitted-code execution, sandbox, runtime, fd 3,
  broker, VM, private object, flag, or intended vulnerability.
- Maximum three OpenCode cycles.
- Never claim a test passed unless you ran it.
- Never discard user work or run reset --hard / clean / force push.

Step 1: run preflight:
pwd
git rev-parse --show-toplevel
git status --short
git branch --show-current
git remote -v
go version
git --version
docker version
docker compose version
opencode --version
opencode agent list

Step 2: read fully:
AGENTS.md
HUSTack_Trusted_Runtime_Challenge_Design.md
00_MASTER_PROMPT.md
PHASES.md
01_PHASE_1_PROMPT.md
docs/phase-1-security-notes.md if present

Step 3: baseline:
gofmt -l .
go mod tidy
git diff -- go.mod go.sum
go vet ./...
go build ./...
go test ./...
docker compose config
git diff --check

Write actual results to `docs/agent_handoff/CODEX_BASELINE.md`.

Step 4: review queue, ratelimit, submissions, database, mock-worker, web,
tests, Compose, and Makefile. Prioritize:
1. queue item contains submission_id and participant_id;
2. claim failure cannot lose a job;
3. FIFO;
4. queued/running quotas are separate;
5. running quota is enforced at claim;
6. two workers cannot exceed one running job per participant;
7. no short TTL destroys critical state;
8. Complete/Fail/Recover are idempotent;
9. stale recovery cannot race DB and Redis;
10. missing lease cannot leave stuck jobs;
11. counters cannot become negative;
12. HTTP queue/quota statuses are correct;
13. real Redis/PostgreSQL tests exist;
14. readyz checks dependencies;
15. oversized requests always return 413;
16. static/404 requests do not create participant rows.

Classify Critical, High, Medium, Test gap.

Step 5: create `docs/agent_handoff/OPENCODE_TASK_01.md`.
Cycle 1 scope only:
- self-contained queue items;
- FIFO;
- running quota at claim;
- lossless claim failure;
- idempotent Complete/Fail;
- focused queue lifecycle tests.

Include objective, current defect, required behavior, expected files,
invariants, tests, commands, forbidden scope, acceptance criteria, and
report path `docs/agent_handoff/OPENCODE_REPORT_01.md`.

Step 6: invoke synchronously:
./scripts/run-opencode-task.sh \
  docs/agent_handoff/OPENCODE_TASK_01.md \
  docs/agent_handoff/OPENCODE_REPORT_01.md \
  docs/agent_handoff/logs/opencode-cycle-01.jsonl

Step 7: read the report, inspect every changed line, and independently run:
git status --short
git diff --stat
git diff --check
git diff
gofmt -l .
go mod tidy
git diff -- go.mod go.sum
go vet ./...
go build ./...
go test ./...
docker compose config
git diff --check

Write `docs/agent_handoff/CODEX_REVIEW_01.md` with summary, accepted
changes, findings, test gaps, actual commands/results, and PASS or FAIL.

If FAIL, create narrowly scoped task 02, then task 03 only if needed.

Commit locally only when scoped changes pass formatting, tidy, vet, build,
tests, relevant queue tests, and diff check. Use:
`fix(phase1): harden queue lifecycle and worker coordination`

Do not push. Without full healthy Docker integration, call it a remediation
checkpoint, not Phase 1 acceptance.

Final report: baseline, cycles, changed files, findings/fixes, actual
commands, passed/skipped tests, commit hash, and one status:
Phase 1 rejected / remediation in progress / accepted.
Only use accepted after healthy Docker plus real PostgreSQL/Redis tests.

Begin with Step 1.
