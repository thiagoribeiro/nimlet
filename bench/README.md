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

## 4. Cluster: horizontal scaling, elasticity, failures

`bench/cluster.sh` runs N Nimlet instances (static binary in `alpine` containers, host
networking) behind HAProxy on `127.0.0.1:18080`, on top of `bench/infra.sh`. HAProxy has eight
pre-declared slots switched through its runtime API, so instances join and leave without a
reload. Rootless podman doesn't delegate the `cpuset` controller, so `bench/pin.sh` pins
containers with `taskset` instead.

```bash
export PG_PORT=15432 REDIS_PORT=16379   # every harness port stays out of 32768-60999 (see below)
HOSTNET=1 NO_S3=1 PG_CPUS=0-2 REDIS_CPUS=7 bench/infra.sh up
bench/cluster.sh build && bench/cluster.sh up 2      # instances on 5-6, HAProxy on 3
go run ./bench/loadgen -addr http://127.0.0.1:18080 -members 2000 -rooms 100 -listeners 2000 \
  -senders 100 -steps 200,400,800 -duration 20s -out /tmp/run
bench/cluster.sh start 3 | remove 3 | kill 2 | lb "show stat" | down
```

Ports sit below the kernel's ephemeral range (32768-60999). With tens of thousands of client
connections, an outgoing socket can take a port in that range before a restarted instance or
database binds it, and the restart fails with "address already in use".

`bench/horizontal.sh <name> [sizes scale elastic redistrib faults]` runs the phase-04 experiments
into `bench/results/<name>/`. A full run takes about three hours.

`bench/batching.sh [ram] [disk] [profile]` runs the phase-05 write-path variants on top of
`horizontal.sh` (`VARIANTS='A B'` picks some), and `bench/pgprofile.sh <dir>` reports Postgres
milliseconds per send from `pg_stat_statements`.

Memory: run `bench/memsample.sh mem.csv &` next to any of them and kill it afterwards.
`python3 bench/memreport.py mem.csv bench/results/<name>` turns a `sizes` run into the
phase-07 tables. `bench/syncramp.sh <name>` holds one instance at a growing number of open
long-polls (`RAMP`, default up to 16k and back) with the sampler running, and
`python3 bench/syncramp_report.py bench/results/<name>` builds its table.

`horizontal.sh` sets `PG_RAM=1`, which keeps the Postgres data directory in `/dev/shm`. Commits
still fsync, but at memory speed. On the laptop disk used for phase 04, fsync tails of about 1s
stalled every commit at once and failed steps at random (`bench/results/04-horizontal/fsync`).
Set `PG_RAM=` to measure with your own disk.

The loadgen reports per step and for the whole run:

- **Steps.** `-steps` runs several rates after a single setup. A step passes when it reached ≥98%
  of its rate, had no failed sends, delivered everything and kept delivery p99 under `-slo`. The
  highest passing step is the *capacity*.
- **Reliability.** After the run, every listener drains its cursor and is reconciled against the
  acked sends of its room:
  - *lost*: acked, never delivered
  - *late*: only found in that final drain
  - *redelivered*: same event twice
  - *double-insert*: one txnId stored twice
  - *reordered*: `room_seq` going backwards

  Failed sends are retried with the same txnId (`-send-retries`). `bench/verify.sql` checks the
  stored events for duplicates directly in Postgres.
- **Time series.** `-out` writes `<out>.ts.jsonl` (per second) and `<out>.summary.json`, so
  failures and scaling events (`events.txt`, unix seconds) can be lined up with latency and
  errors.
- **Setup reuse.** `-setup-only -state f.json` runs the argon2 registrations once, against an
  unlimited instance (`cluster.sh start 9`, off the LB). `-state f.json` reuses that setup in
  later runs.
