#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

NORMAL="docker compose"
ACCEPT="docker compose -p hustack-phase1-accept -f docker-compose.yml -f tests/docker-compose.acceptance.yml"

cleanup() {
    $ACCEPT down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

run_no_skip() {
    output=$(mktemp)
    if ! "$@" >"$output" 2>&1; then
        cat "$output"
        rm -f "$output"
        return 1
    fi
    cat "$output"
    if grep -Ei -- '---[[:space:]]+SKIP:|redis[[:space:]]+(unavailable|not available)|postgres(ql)?[[:space:]]+(unavailable|not available)|connection refused' "$output"; then
        rm -f "$output"
        echo "required test output contains a skip/unavailable marker" >&2
        return 1
    fi
    rm -f "$output"
}

wait_healthy() {
    attempts=0
    while [ "$attempts" -lt 60 ]; do
        unhealthy=$($ACCEPT ps --format json | grep -E '"Health":"(starting|unhealthy)"' || true)
        count=$($ACCEPT ps --services --status running | wc -l)
        if [ -z "$unhealthy" ] && [ "$count" -eq 5 ]; then
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 1
    done
    $ACCEPT ps
    return 1
}

wait_normal_healthy() {
    attempts=0
    while [ "$attempts" -lt 60 ]; do
        unhealthy=$($NORMAL ps --format json | grep -E '"Health":"(starting|unhealthy)"' || true)
        count=$($NORMAL ps --services --status running | wc -l)
        if [ -z "$unhealthy" ] && [ "$count" -eq 5 ]; then
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 1
    done
    $NORMAL ps
    return 1
}

$ACCEPT down -v --remove-orphans
$NORMAL down -v --remove-orphans
$NORMAL build --no-cache api mock-worker
$NORMAL up -d --force-recreate
wait_normal_healthy
$NORMAL ps

go vet ./...
go build ./...
go test ./...
go test -race ./...
git diff --check
docker compose config --quiet
make integration-test

$NORMAL down -v --remove-orphans
ACCEPT_QUEUE_MAX_DEPTH=50 ACCEPT_SUBMIT_RATE_PER_MINUTE=2 $ACCEPT up -d --force-recreate
wait_healthy
run_no_skip env DATABASE_URL=postgres://hustack:hustack@127.0.0.1:25432/hustack?sslmode=disable HUSTACK_REQUIRE_POSTGRES=1 go test -v ./internal/database -count=1
run_no_skip env DATABASE_URL=postgres://hustack:hustack@127.0.0.1:25432/hustack?sslmode=disable HUSTACK_REQUIRE_POSTGRES=1 go test -race ./internal/database -count=1
run_no_skip env REDIS_ADDR=127.0.0.1:26379 HUSTACK_REQUIRE_REDIS=1 go test -v ./internal/queue -count=1
run_no_skip env REDIS_ADDR=127.0.0.1:26379 HUSTACK_REQUIRE_REDIS=1 go test -race ./internal/queue -count=1
run_no_skip env REDIS_ADDR=127.0.0.1:26379 HUSTACK_REQUIRE_REDIS=1 go test -v ./internal/ratelimit -count=1
run_no_skip env REDIS_ADDR=127.0.0.1:26379 HUSTACK_REQUIRE_REDIS=1 go test -race ./internal/ratelimit -count=1
run_no_skip go test -v -tags=phase1acceptance ./tests -run 'TestApplicationParticipantRateLimit|TestNginxSubmissionRateLimit|TestActualHostileResultUsesTextOnlySink' -count=1

ACCEPT_QUEUE_MAX_DEPTH=1 ACCEPT_SUBMIT_RATE_PER_MINUTE=100 $ACCEPT up -d --force-recreate api mock-worker
wait_healthy
run_no_skip go test -v -tags=phase1acceptance ./tests -run TestGlobalCapacityRejectsWithoutPartialState -count=1

docker image inspect --format '{{.Id}}' 04-api
docker image inspect --format '{{.Id}}' 04-mock-worker
$ACCEPT ps

$ACCEPT down -v --remove-orphans
$NORMAL up -d --force-recreate
wait_normal_healthy
$NORMAL ps
