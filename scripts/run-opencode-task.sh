#!/usr/bin/env bash
set -Eeuo pipefail
[ "$#" -eq 3 ] || { echo "Usage: $0 task.md report.md log.jsonl"; exit 2; }

TASK="$1"
REPORT="$2"
LOG="$3"
test -f "$TASK" || { echo "Task not found: $TASK"; exit 1; }
mkdir -p "$(dirname "$REPORT")" "$(dirname "$LOG")"

PROMPT="$(cat "$TASK")

Read the task completely. Implement only its scope. Add required tests.
Run required Go checks. Write the factual report to $REPORT.
Do not commit, push, or start another task."

ARGS=(run)
opencode run --help 2>&1 | grep -q -- '--agent' && ARGS+=(--agent builder)
opencode run --help 2>&1 | grep -q -- '--format' && ARGS+=(--format json)

opencode "${ARGS[@]}" "$PROMPT" | tee "$LOG"
