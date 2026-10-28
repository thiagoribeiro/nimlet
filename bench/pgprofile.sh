#!/bin/sh
# Where Postgres' time goes in one constant-rate run: pg_stat_statements over a fresh
# database, N small instances behind the LB (same pinning as bench/horizontal.sh).
#   bench/pgprofile.sh OUT_DIR
# Knobs (env): N (8), RATE (4000), DUR (30s), WL (send-heavy loadgen flags), plus NIMLET_ENV,
# CPU_PER_INSTANCE and PG_RAM as in horizontal.sh.
# Output: OUT_DIR/lg.* (loadgen), top.txt (statements by total time), per-send.txt.
set -eu
cd "$(dirname "$0")/.."
OUT=$1
mkdir -p "$OUT"
export PG_PORT=${PG_PORT:-15432} REDIS_PORT=${REDIS_PORT:-16379}
export PG_CPUS=${PG_CPUS:-0-2} REDIS_CPUS=${REDIS_CPUS:-7} LB_CPUS=${LB_CPUS:-3} INST_CPUS=${INST_CPUS:-5-6}
export HOSTNET=1 NO_S3=1 PG_LOCKS=${PG_LOCKS:-256} PG_RAM=${PG_RAM-1} PG_STATS=1
export NIMLET_BENCH_PG_IMAGE=${NIMLET_BENCH_PG_IMAGE:-docker.io/library/postgres:18-alpine}
export CPU_PER_INSTANCE=${CPU_PER_INSTANCE-0.25} NIMLET_ENV=${NIMLET_ENV-NIMLET_PROFILE=small}
WL=${WL:--members 200 -rooms 20 -listeners 20 -senders 200}
RT=$(command -v docker || command -v podman)
LB=http://127.0.0.1:18080
STATE=bin/bench-state.json

psql() { "$RT" exec -i nimlet-bench-pg psql -U nimlet -d nimlet_bench -p "$PG_PORT" "$@"; }

go build -o bin/loadgen ./bench/loadgen
bench/cluster.sh build
bench/cluster.sh down
bench/infra.sh up 2>/dev/null
psql -qc "CREATE EXTENSION pg_stat_statements"
bench/cluster.sh start 9 2>/dev/null
# shellcheck disable=SC2086
bin/loadgen -setup-addr http://127.0.0.1:18089 -addr "$LB" $WL -setup-concurrency 8 -setup-only -state "$STATE" >/dev/null
bench/cluster.sh stop 9
bench/cluster.sh up "${N:-8}"
psql -qAt -c "SELECT pg_stat_statements_reset()" >/dev/null
bench/cluster.sh stats "$OUT/stats" &
sampler=$!
# shellcheck disable=SC2086
taskset -c "${LG_CPUS:-4,7}" bin/loadgen -addr "$LB" -state "$STATE" -out "$OUT/lg" $WL \
	-rate "${RATE:-4000}" -duration "${DUR:-30s}" > "$OUT/lg.txt" 2>&1 || true
kill "$sampler" 2>/dev/null || true

psql -At -F' | ' -c "
	SELECT round((total_exec_time + total_plan_time)::numeric) AS ms, calls,
	       round(((total_exec_time + total_plan_time) / nullif(calls, 0))::numeric, 3) AS ms_per_call,
	       left(regexp_replace(query, '\s+', ' ', 'g'), 120)
	FROM pg_stat_statements ORDER BY 1 DESC LIMIT 20" > "$OUT/top.txt"
# The warm-up sends count too: the stats were reset before it.
sends=$(sed -n 's/^sends: attempted=[0-9]* acked=\([0-9]*\).*/\1/p' "$OUT/lg.txt")
psql -At -v sends="${sends:-0}" > "$OUT/per-send.txt" <<'SQL'
SELECT 'sends ' || :sends;
SELECT 'postgres ms per send ' || round((sum(total_exec_time + total_plan_time) / nullif(:sends, 0))::numeric, 3)
FROM pg_stat_statements;
SELECT 'events per insert batch ' || round((:sends::numeric / nullif(sum(calls), 0)), 1)
FROM pg_stat_statements WHERE query LIKE '%WITH input AS%';
SQL
bench/cluster.sh down
bench/infra.sh down 2>/dev/null
