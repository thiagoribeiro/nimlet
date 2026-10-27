#!/bin/sh
# Ephemeral Postgres + Redis for benchmarks (no volumes; `up` always starts from an empty DB).
#   bench/infra.sh up     # recreate and wait until ready
#   bench/infra.sh down
# Ports: Postgres 55432, Redis 56379, S3 (SeaweedFS) 59000 — chosen to avoid clashing with
# `make docker-up`. PG_PORT / REDIS_PORT override the first two.
set -eu
RT=$(command -v docker || command -v podman)
PG=nimlet-bench-pg
RD=nimlet-bench-redis
S3=nimlet-bench-s3
PG_PORT=${PG_PORT:-55432}
REDIS_PORT=${REDIS_PORT:-56379}

down() { "$RT" rm -f "$PG" "$RD" "$S3" >/dev/null 2>&1 || true; }

case "${1:-}" in
up)
	down
	# PG_CPUS / REDIS_CPUS pin the containers (bench/pin.sh, used by bench/cluster.sh); unset = no pinning.
	# HOSTNET=1 uses host networking on the same ports: rootless port forwarding adds latency
	# that would dominate the multi-instance runs.
	if [ -n "${HOSTNET:-}" ]; then
		pgnet="--network host" pgport="-c port=$PG_PORT" pgready=$PG_PORT rdnet="--network host" rdport="--port $REDIS_PORT"
	else
		pgnet="-p $PG_PORT:5432" pgport="" pgready=5432 rdnet="-p $REDIS_PORT:6379" rdport=""
	fi
	# PG_STATS=1 loads pg_stat_statements (CREATE EXTENSION pg_stat_statements to read it).
	if [ -n "${PG_STATS:-}" ]; then
		pgport="$pgport -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=all -c pg_stat_statements.track_planning=on"
	fi
	# PG_TUNE=1: a sized config instead of the image defaults (128MB shared_buffers, 1GB
	# max_wal_size, JIT on), which make backends flush their own dirty pages and force
	# checkpoints (and full-page writes) every few hundred MB of sends.
	# PG_LOCKS=n sets max_locks_per_transaction; on Postgres 18 it also sizes the per-backend
	# fast-path lock slots (16 before 18), which a /sync query over partitioned tables exceeds.
	if [ -n "${PG_LOCKS:-}" ]; then
		pgport="$pgport -c max_locks_per_transaction=$PG_LOCKS"
	fi
	if [ -n "${PG_TUNE:-}" ]; then
		pgport="$pgport -c shared_buffers=2GB -c effective_cache_size=4GB -c max_wal_size=8GB \
			-c checkpoint_timeout=15min -c wal_buffers=64MB -c jit=off -c random_page_cost=1.1"
	fi
	# shellcheck disable=SC2086
	"$RT" run -d --rm --name "$PG" $pgnet -e POSTGRES_USER=nimlet -e POSTGRES_PASSWORD=nimlet \
		-e POSTGRES_DB=nimlet_bench "${NIMLET_BENCH_PG_IMAGE:-docker.io/library/postgres:16-alpine}" \
		-c max_connections=300 $pgport >/dev/null
	# shellcheck disable=SC2086
	"$RT" run -d --rm --name "$RD" $rdnet docker.io/library/redis:7-alpine \
		redis-server $rdport >/dev/null
	# S3-compatible store for the S3 media adapter tests (TEST_S3_ENDPOINT=http://127.0.0.1:59000).
	# NO_S3=1 skips it (the cluster benchmarks don't use media).
	if [ -z "${NO_S3:-}" ]; then
		"$RT" run -d --rm --name "$S3" -p 59000:8333 docker.io/chrislusf/seaweedfs:latest server -s3 >/dev/null
	fi
	for _ in $(seq 1 60); do
		if "$RT" exec "$PG" pg_isready -U nimlet -q -p "$pgready" 2>/dev/null; then
			sleep 1
			if [ -n "${PG_CPUS:-}" ]; then "$(dirname "$0")/pin.sh" "$PG" "$PG_CPUS"; fi
			if [ -n "${REDIS_CPUS:-}" ]; then "$(dirname "$0")/pin.sh" "$RD" "$REDIS_CPUS"; fi
			exit 0
		fi
		sleep 1
	done
	echo "postgres not ready" >&2
	exit 1
	;;
down) down ;;
*) echo "usage: $0 up|down" >&2; exit 2 ;;
esac
