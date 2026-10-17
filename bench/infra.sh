#!/bin/sh
# Ephemeral Postgres + Redis for benchmarks (no volumes; `up` always starts from an empty DB).
#   bench/infra.sh up     # recreate and wait until ready
#   bench/infra.sh down
# Ports: Postgres 55432, Redis 56379, S3 (SeaweedFS) 59000 — chosen to avoid clashing with
# `make docker-up`.
set -eu
RT=$(command -v docker || command -v podman)
PG=nimlet-bench-pg
RD=nimlet-bench-redis
S3=nimlet-bench-s3

down() { "$RT" rm -f "$PG" "$RD" "$S3" >/dev/null 2>&1 || true; }

case "${1:-}" in
up)
	down
	"$RT" run -d --rm --name "$PG" -e POSTGRES_USER=nimlet -e POSTGRES_PASSWORD=nimlet \
		-e POSTGRES_DB=nimlet_bench -p 55432:5432 "${NIMLET_BENCH_PG_IMAGE:-docker.io/library/postgres:16-alpine}" \
		-c max_connections=300 >/dev/null
	"$RT" run -d --rm --name "$RD" -p 56379:6379 docker.io/library/redis:7-alpine >/dev/null
	# S3-compatible store for the S3 media adapter tests (TEST_S3_ENDPOINT=http://127.0.0.1:59000)
	"$RT" run -d --rm --name "$S3" -p 59000:8333 docker.io/chrislusf/seaweedfs:latest server -s3 >/dev/null
	for _ in $(seq 1 60); do
		if "$RT" exec "$PG" pg_isready -U nimlet -q 2>/dev/null; then sleep 1; exit 0; fi
		sleep 1
	done
	echo "postgres not ready" >&2
	exit 1
	;;
down) down ;;
*) echo "usage: $0 up|down" >&2; exit 2 ;;
esac
