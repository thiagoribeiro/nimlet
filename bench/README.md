# Benchmarks

Three layers, all reproducible on a laptop. Results and their history live in
[`docs/benchmarks.md`](../docs/benchmarks.md); raw outputs per phase in `bench/results/`.

## 1. Infra

```bash
bench/infra.sh up        # fresh Postgres :55432 + Redis :56379 (docker or podman)
export TEST_POSTGRES_DSN="postgres://nimlet:nimlet@localhost:55432/nimlet_bench?sslmode=disable"
export TEST_REDIS_ADDR="localhost:56379"
```

Always start from `up`: the Postgres fixture adds ~70k events per run and numbers drift on a
reused database.

## 2. Go benchmarks

```bash
# Store-level hot paths against real Postgres (fixture built once per run, see bench_integration_test.go)
go test -run '^$' -bench . -benchmem -count 6 -benchtime 2s -tags integration ./internal/adapter/postgres/
# In-memory long-poll registry
go test -run '^$' -bench . -benchmem -count 6 -benchtime 300ms ./internal/sync/
```

Compare two runs with `go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt`.

## 3. End-to-end load (`bench/loadgen`)

```bash
make build
NIMLET_DB_DSN="$TEST_POSTGRES_DSN" NIMLET_REDIS_ADDR=localhost:56379 NIMLET_HTTP_ADDR=:58080 \
NIMLET_PPROF_ADDR=localhost:56060 NIMLET_AUTH_RATE_PER_MIN=100000 NIMLET_AUTH_HMAC_SECRET=dev-secret-at-least-32-chars-long!! \
  ./bin/nimlet server &

# small room: per-user fan-out path
go run ./bench/loadgen -addr http://localhost:58080 -members 200 -listeners 200 -senders 10 -rate 200 -duration 30s
# large room (>=1000 members): room-key wake-up path
go run ./bench/loadgen -addr http://localhost:58080 -members 1100 -listeners 300 -senders 10 -rate 50 -duration 30s -setup-concurrency 6
```

`bench/run-all.sh <name>` runs the whole sequence (fresh infra, Go benchmarks, moderate and
saturating load) into `bench/results/<name>/`.

`delivery` is send → event received by a `/sync` long-poll, measured with the sender's
timestamp embedded in the message. `delivered` below 100% means events were **skipped** by a
client cursor, not slow — every scenario should report 100%.

CPU profile while a scenario runs: `curl -o cpu.pprof 'localhost:56060/debug/pprof/profile?seconds=10'`.
