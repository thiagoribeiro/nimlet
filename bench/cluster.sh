#!/bin/sh
# Multi-instance Nimlet behind HAProxy, on top of bench/infra.sh's Postgres + Redis.
#   bench/cluster.sh build          # static binary bin/nimlet-static (mounted into the containers)
#   bench/cluster.sh up N           # HAProxy + instances 1..N, each enabled in the LB once listening
#   bench/cluster.sh start K        # start instance K (1..8 join the LB; 9 is the off-LB setup instance)
#   bench/cluster.sh drain K        # LB stops sending K new requests; waits for its sessions to end
#   bench/cluster.sh stop K         # SIGTERM (graceful) and take K out of the LB
#   bench/cluster.sh remove K       # drain + stop
#   bench/cluster.sh kill K         # SIGKILL, LB untouched (it notices through its health check)
#   bench/cluster.sh stats DIR      # sample per-core CPU and HAProxy counters every second (until killed)
#   bench/cluster.sh lb CMD...      # raw HAProxy runtime API command, e.g. "show stat"
#   bench/cluster.sh down
# Instance K listens on :1808K (metrics on 127.0.0.1:1818K); the LB on 127.0.0.1:18080.
# All containers use host networking: rootless port forwarding would dominate the latencies.
#
# CPU placement (8-thread laptop; see docs/benchmarks.md, 04):
#   INST_CPUS        CPUs shared by all instances (bench/pin.sh) (default 5-7)
#   CPU_PER_INSTANCE --cpus quota per instance; empty = none  (default empty)
#   LB_CPUS          CPUs for HAProxy                          (default 3)
#   DB_CONNS         NIMLET_DB_MAX_CONNS per instance         (default: the instance profile's)
#   NIMLET_ENV       extra "KEY=VALUE ..." passed to every instance (e.g. NIMLET_EVENT_BATCH_MAX=1)
# PG_PORT / REDIS_PORT must match the ones bench/infra.sh was started with.
set -eu
cd "$(dirname "$0")/.."
RT=$(command -v docker || command -v podman)
LB=nimlet-bench-lb
INST_CPUS=${INST_CPUS:-5-7}
CPU_PER_INSTANCE=${CPU_PER_INSTANCE:-}
LB_CPUS=${LB_CPUS:-3}
DB_CONNS=${DB_CONNS:-}
PG_PORT=${PG_PORT:-55432}
REDIS_PORT=${REDIS_PORT:-56379}
DSN="postgres://nimlet:nimlet@localhost:$PG_PORT/nimlet_bench?sslmode=disable"

lb() { echo "$*" | socat stdio tcp4-connect:127.0.0.1:19999; }

lb_up() {
	"$RT" rm -f "$LB" >/dev/null 2>&1 || true
	"$RT" run -d --rm --name "$LB" --network host \
		-v "$PWD/bench/haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
		docker.io/library/haproxy:3.0-alpine >/dev/null
	bench/pin.sh "$LB" "$LB_CPUS"
	for _ in $(seq 1 50); do lb "show info" >/dev/null 2>&1 && return 0; sleep 0.2; done
	echo "haproxy not ready" >&2
	exit 1
}

start() {
	k=$1
	"$RT" rm -f "nimlet-bench-n$k" >/dev/null 2>&1 || true
	# No --rm: a crashed instance keeps its logs until "down" or the next start removes it.
	# Instance 9 only runs setup (argon2 registrations), so it gets no CPU limits.
	limits=""
	# The quota uses a 10ms period instead of the default 100ms: a throttled instance then
	# pauses for a few ms, not ~75ms, which would dominate every latency it reports.
	if [ "$k" -le 8 ] && [ -n "$CPU_PER_INSTANCE" ]; then
		limits="--cpu-period 10000 --cpu-quota $(awk -v c="$CPU_PER_INSTANCE" 'BEGIN { printf "%d", c * 10000 }')"
	fi
	extra=""
	for kv in ${NIMLET_ENV:-}; do extra="$extra -e $kv"; done
	# shellcheck disable=SC2086
	"$RT" run -d --name "nimlet-bench-n$k" --network host $limits $extra \
		-v "$PWD/bin/nimlet-static:/nimlet:ro" \
		-e NIMLET_DB_DSN="$DSN" -e NIMLET_REDIS_ADDR="localhost:$REDIS_PORT" \
		-e NIMLET_HTTP_ADDR=":1808$k" -e NIMLET_METRICS_ADDR="127.0.0.1:1818$k" \
		${DB_CONNS:+-e NIMLET_DB_MAX_CONNS=$DB_CONNS} -e NIMLET_AUTH_RATE_PER_MIN=1000000 \
		-e NIMLET_AUTH_HMAC_SECRET='dev-secret-at-least-32-chars-long!!' \
		-e NIMLET_MEDIA_PATH=/tmp/media \
		docker.io/library/alpine:3.21 /nimlet server >/dev/null
	if [ "$k" -le 8 ]; then bench/pin.sh "nimlet-bench-n$k" "$INST_CPUS"; fi
	# "Ready" is the HTTP port answering, which is all a TCP health check sees too.
	for _ in $(seq 1 100); do
		code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:1808$k/sync" || true)
		if [ "$code" != "000" ]; then
			if [ "$k" -le 8 ]; then lb "set server nimlet/n$k state ready" >/dev/null; fi
			return 0
		fi
		sleep 0.2
	done
	echo "instance $k not ready" >&2
	"$RT" logs "nimlet-bench-n$k" >&2 || true
	exit 1
}

drain() {
	lb "set server nimlet/n$1 state drain" >/dev/null
	# Long-polls end within the client's /sync timeout; don't wait longer than that.
	for _ in $(seq 1 "${DRAIN_WAIT:-35}"); do
		cur=$(lb "show stat" | awk -F, -v s="n$1" '$1=="nimlet" && $2==s {print $5}')
		[ "${cur:-0}" = "0" ] && return 0
		sleep 1
	done
}

stop() {
	"$RT" stop -t 35 "nimlet-bench-n$1" >/dev/null 2>&1 || true
	lb "set server nimlet/n$1 state maint" >/dev/null 2>&1 || true
}

# stats samples busy % per core (cores map to components through the pinning above; podman
# stats under-reports rootless containers) and the HAProxy counters, once per second.
stats() {
	mkdir -p "$1"
	echo "ts,server,scur,stot,econ,eresp,wretr,wredis,status" > "$1/haproxy.csv"
	echo "ts,load1,load5,load15" > "$1/loadavg.csv"
	echo "ts,core,busy_pct" > "$1/cores.csv"
	prev=$(grep '^cpu[0-9]' /proc/stat)
	while :; do
		sleep 1
		ts=$(date +%s)
		cur=$(grep '^cpu[0-9]' /proc/stat)
		printf '%s\n%s\n' "$prev" "$cur" | awk -v ts="$ts" '{
			idle = $5 + $6; tot = 0; for (i = 2; i <= NF; i++) tot += $i
			if ($1 in T) { d = tot - T[$1]; printf "%s,%s,%.0f\n", ts, substr($1, 4), d ? 100 * (1 - (idle - I[$1]) / d) : 0 }
			else { T[$1] = tot; I[$1] = idle }
		}' >> "$1/cores.csv"
		prev=$cur
		# Background load from outside the benchmark shows up here (the host isn't isolated).
		awk -v ts="$ts" '{print ts","$1","$2","$3}' /proc/loadavg >> "$1/loadavg.csv"
		lb "show stat" 2>/dev/null | awk -F, -v ts="$ts" '$1=="nimlet" {print ts","$2","$5","$8","$14","$15","$16","$17","$18}' \
			>> "$1/haproxy.csv" || true
	done
}

case "${1:-}" in
build) CGO_ENABLED=0 go build -trimpath -o bin/nimlet-static ./cmd/nimlet ;;
up)
	lb_up
	start 1 # alone first: it applies the migrations
	pids=""
	for k in $(seq 2 "$2"); do
		start "$k" &
		pids="$pids $!"
	done
	# A missing instance would silently skew every result: fail the whole "up".
	for p in $pids; do wait "$p" || { echo "cluster up: an instance failed to start" >&2; exit 1; }; done
	;;
start) start "$2" ;;
drain) drain "$2" ;;
stop) stop "$2" ;;
remove) drain "$2"; stop "$2" ;;
kill) "$RT" kill -s KILL "nimlet-bench-n$2" >/dev/null ;;
stats) stats "$2" ;;
lb) shift; lb "$*" ;;
down)
	for k in $(seq 1 9); do "$RT" rm -f "nimlet-bench-n$k" >/dev/null 2>&1 || true; done
	"$RT" rm -f "$LB" >/dev/null 2>&1 || true
	;;
*) echo "usage: $0 build|up N|start K|drain K|stop K|remove K|kill K|stats DIR|lb CMD|down" >&2; exit 2 ;;
esac
