#!/bin/sh
# Full benchmark sequence used for docs/benchmarks.md: fresh infra → Go benchmarks →
# server → both load scenarios. Usage: bench/run-all.sh <results-dir-name>
set -eu
cd "$(dirname "$0")/.."
OUT="bench/results/$1"
mkdir -p "$OUT"
export TEST_POSTGRES_DSN="postgres://nimlet:nimlet@localhost:55432/nimlet_bench?sslmode=disable"
export TEST_REDIS_ADDR="localhost:56379"

bench/infra.sh up
go test -run '^$' -bench . -benchmem -count 6 -benchtime 2s -tags integration ./internal/adapter/postgres/ > "$OUT/postgres.txt" 2>&1
go test -run '^$' -bench . -benchmem -count 6 -benchtime 300ms ./internal/sync/ > "$OUT/sync.txt" 2>&1

go build -o bin/nimlet ./cmd/nimlet
NIMLET_DB_DSN="$TEST_POSTGRES_DSN" NIMLET_REDIS_ADDR="$TEST_REDIS_ADDR" NIMLET_HTTP_ADDR=:58080 \
NIMLET_PPROF_ADDR=localhost:56060 NIMLET_AUTH_RATE_PER_MIN=100000 NIMLET_AUTH_HMAC_SECRET=dev-secret-at-least-32-chars-long!! \
NIMLET_MEDIA_PATH="$PWD/bin/media" ./bin/nimlet server > bin/bench-server.log 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT
for _ in $(seq 1 40); do curl -sf localhost:58080/metrics >/dev/null && break; sleep 0.5; done

# Moderate load: the normal operating regime (latency, not capacity).
{
	go run ./bench/loadgen -addr http://localhost:58080 -members 200 -listeners 200 -senders 10 -rate 20 -duration 30s -prefix msmall
	go run ./bench/loadgen -addr http://localhost:58080 -members 50 -listeners 50 -senders 5 -rate 100 -duration 30s -prefix mchat
} > "$OUT/e2e-moderate.txt" 2>&1
# Saturation: every listener long-polls in a tight loop; measures capacity under the wake-up herd.
go run ./bench/loadgen -addr http://localhost:58080 -members 200 -listeners 200 -senders 10 -rate 200 \
	-duration 30s -prefix small > "$OUT/e2e-small.txt" 2>&1
go run ./bench/loadgen -addr http://localhost:58080 -members 1100 -listeners 300 -senders 10 -rate 50 \
	-duration 30s -setup-concurrency 6 -prefix large > "$OUT/e2e-large.txt" 2>&1
cat "$OUT"/e2e-*.txt
