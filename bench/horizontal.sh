#!/bin/sh
# Horizontal-scaling experiments for docs/benchmarks.md (phase 04), on bench/cluster.sh.
#   bench/horizontal.sh <results-dir-name> [sizes|scale|elastic|redistrib|faults ...]   (default: all)
# Output: bench/results/<name>/<experiment>/... (loadgen txt + .ts.jsonl + .summary.json,
# container/HAProxy samples, verify.sql output, events.txt with unix-second markers).
#
# Knobs (env): REPS (3), STEP_DURATION (20s), SYNC_STEPS / SEND_STEPS / REF_STEPS (rates per
# step), ELASTIC_RATE / FAULT_RATE (msg/s of the constant-load experiments), LG_CPUS (loadgen,
# 4,7), PG_CPUS (0-2), REDIS_CPUS (7), plus bench/cluster.sh's INST_CPUS (5-6), LB_CPUS (3) and
# CPU_PER_INSTANCE (0.25). Postgres is 18 with max_locks_per_transaction=256 (docs, phase 04).
# A/B knobs (phase 05): VARIANT_ENV ("KEY=VALUE ..." added to every instance on top of the
# profile), SCALE_WORKLOADS ("sync send"), SCALE_NS ("1 2 4 8") and FAULTS (names, default all).
set -eu
cd "$(dirname "$0")/.."
NAME=$1
shift
EXPERIMENTS=${*:-sizes scale elastic redistrib faults}
OUT="bench/results/$NAME"
mkdir -p "$OUT"
RT=$(command -v docker || command -v podman)

# Outside the kernel ephemeral range (32768-60999): with tens of thousands of client
# connections, a port in that range can be taken by an outgoing socket before a restarted
# server binds it.
export PG_PORT=${PG_PORT:-15432} REDIS_PORT=${REDIS_PORT:-16379}
export PG_CPUS=${PG_CPUS:-0-2} REDIS_CPUS=${REDIS_CPUS:-7} LB_CPUS=${LB_CPUS:-3} INST_CPUS=${INST_CPUS:-5-6}
export HOSTNET=1 NO_S3=1 PG_LOCKS=${PG_LOCKS:-256} PG_RAM=${PG_RAM-1}
export NIMLET_BENCH_PG_IMAGE=${NIMLET_BENCH_PG_IMAGE:-docker.io/library/postgres:18-alpine}
# One unlimited instance saturates Postgres' three cores, so the cluster experiments give each
# instance a quota (0.25 CPU, the "small" profile) that makes N matter: 8 of them fill the two
# instance cores that the single-instance reference gets whole.
export CPU_PER_INSTANCE=${CPU_PER_INSTANCE-0.25}
VARIANT_ENV=${VARIANT_ENV:-}
export NIMLET_ENV="${NIMLET_ENV-NIMLET_PROFILE=small} $VARIANT_ENV"
LG_CPUS=${LG_CPUS:-4,7}
REPS=${REPS:-3}
STEP_DURATION=${STEP_DURATION:-20s}
SYNC_STEPS=${SYNC_STEPS:-100,150,200,300,400,500,600,800,1000,1300,1600,2000}
SEND_STEPS=${SEND_STEPS:-200,400,600,800,1000,1300,1600,2000,2500,3000,4000,5000,6000}
REF_STEPS=${REF_STEPS:-600,800,1000,1300,1600,2000,2500,3000}
# ~60% of the capacity the pilot runs measured for 2 (elastic) and 4 (faults) small instances.
ELASTIC_RATE=${ELASTIC_RATE:-180}
FAULT_RATE=${FAULT_RATE:-400}
LB=http://127.0.0.1:18080

# Workloads. sync-heavy: 2000 listeners in 100 rooms of 20, every send wakes 20 long-polls
# (cost lands on the instances). send-heavy: 200 senders in 20 rooms, one listener per room
# (cost lands on Postgres' serialised position allocation).
WL_SYNC="-members 2000 -rooms 100 -listeners 2000 -senders 100"
WL_SEND="-members 200 -rooms 20 -listeners 20 -senders 200"

LG=bin/loadgen
go build -o "$LG" ./bench/loadgen
bench/cluster.sh build

# What else was running: the host isn't isolated, so record it with the results.
{
	echo "commit $(git rev-parse --short HEAD) $(date -Is)"
	echo "variant: ${VARIANT_ENV:-none} PG_RAM=${PG_RAM:-}"
	uptime
	top -bn1 -o %CPU | head -20
} > "$OUT/host-$(date +%s).txt"

mark() {
	mdir=$1
	shift
	echo "$(date +%s) $*" >> "$mdir/events.txt"
}

# fresh DIR WORKLOAD: empty database, setup on the unlimited off-LB instance. The state file
# holds access tokens, so it stays in bin/ (gitignored), not with the results.
STATE=bin/bench-state.json
fresh() {
	bench/cluster.sh down
	bench/infra.sh up 2>/dev/null
	bench/cluster.sh start 9 2>/dev/null
	# shellcheck disable=SC2086
	"$LG" -setup-addr http://127.0.0.1:18089 -addr "$LB" $2 -setup-concurrency 8 -setup-only -state "$STATE" >/dev/null
	bench/cluster.sh stop 9
}

# loadgen DIR BASENAME ARGS...: one pinned loadgen run with container/LB sampling around it.
loadgen() {
	dir=$1 base=$2
	shift 2
	bench/cluster.sh stats "$dir/$base" &
	sampler=$!
	taskset -c "$LG_CPUS" "$LG" -addr "$LB" -state "$STATE" -out "$dir/$base" "$@" > "$dir/$base.txt" 2>&1 || true
	kill "$sampler" 2>/dev/null || true
	wait "$sampler" 2>/dev/null || true
	run=$(sed -n 's/.*"run": "\(.*\)".*/\1/p' "$dir/$base.summary.json" 2>/dev/null || true)
	if [ -n "$run" ]; then
		"$RT" exec -i nimlet-bench-pg psql -U nimlet -d nimlet_bench -p "$PG_PORT" -v run="$run" -f - \
			< bench/verify.sql > "$dir/$base.verify.txt" 2>&1 || true
	fi
	grep -E "^(capacity|sends|deliveries|integrity|sync):" "$dir/$base.txt" | sed "s|^|  $base: |" || true
}

# 0. Profiles on the same hardware (the two instance cores): 8 small instances (0.25 CPU
# each), 2 medium (1 CPU each) and 1 large (no quota), REPS repetitions each.
sizes() {
	d="$OUT/sizes"
	mkdir -p "$d"
	for spec in "small 8 0.25" "medium 2 1" "large 1 -"; do
		# shellcheck disable=SC2086
		set -- $spec
		prof=$1 n=$2 quota=$3
		[ "$quota" = - ] && quota=""
		for rep in $(seq 1 "$REPS"); do
			fresh "$d" "$WL_SYNC"
			CPU_PER_INSTANCE="$quota" NIMLET_ENV="NIMLET_PROFILE=$prof $VARIANT_ENV" bench/cluster.sh up "$n"
			# shellcheck disable=SC2086
			loadgen "$d" "$prof-n$n-r$rep" $WL_SYNC -steps "$REF_STEPS" -duration "$STEP_DURATION"
		done
	done
}

# 1. Capacity and latency with 1, 2, 4 and 8 small instances, both workloads, REPS repetitions.
scale() {
	d="$OUT/scale"
	mkdir -p "$d"
	for wl in ${SCALE_WORKLOADS:-sync send}; do
		if [ "$wl" = sync ]; then args=$WL_SYNC steps=$SYNC_STEPS; else args=$WL_SEND steps=$SEND_STEPS; fi
		for n in ${SCALE_NS:-1 2 4 8}; do
			for rep in $(seq 1 "$REPS"); do
				fresh "$d" "$args"
				bench/cluster.sh up "$n"
				# shellcheck disable=SC2086
				loadgen "$d" "$wl-n$n-r$rep" $args -steps "$steps" -duration "$STEP_DURATION"
			done
		done
	done
}

# 2. Constant load while instances join and leave: 2 → 3 → 4 → 3 → 2. "drain" takes an
# instance out of the LB first; "nodrain" just SIGTERMs it.
elastic() {
	d="$OUT/elastic"
	mkdir -p "$d"
	for mode in drain nodrain; do
		fresh "$d" "$WL_SYNC"
		bench/cluster.sh up 2
		: > "$d/events.txt"
		# shellcheck disable=SC2086
		loadgen "$d" "$mode" $WL_SYNC -rate "$ELASTIC_RATE" -duration 300s &
		lgpid=$!
		sleep 60; mark "$d" "$mode start 3"; bench/cluster.sh start 3
		sleep 60; mark "$d" "$mode start 4"; bench/cluster.sh start 4
		for k in 4 3; do
			sleep 60
			if [ "$mode" = drain ]; then
				mark "$d" "$mode drain $k"; bench/cluster.sh drain "$k"
			fi
			mark "$d" "$mode stop $k"; bench/cluster.sh stop "$k"
		done
		wait "$lgpid"
		cp "$d/events.txt" "$d/$mode.events.txt"
	done
}

# 3. Redistribution and reconnection. Low rate so long-polls really wait (each room gets a
# message every ~5s); an instance is added and removed in cycles. A long-poll that lands on a
# new instance before its 256 Redis subscriptions are up misses its wake-up and returns only at
# its timeout, so the run is repeated with two timeouts: a delivery max that follows the
# timeout means the race is real.
redistrib() {
	d="$OUT/redistrib"
	mkdir -p "$d"
	for timeout in 30s 5s; do
		fresh "$d" "$WL_SYNC"
		bench/cluster.sh up 1
		: > "$d/events.txt"
		# shellcheck disable=SC2086
		loadgen "$d" "timeout$timeout" $WL_SYNC -rate 20 -duration 480s -sync-timeout "$timeout" -slo 60s &
		lgpid=$!
		sleep 30
		for cycle in 1 2 3 4 5; do
			# Enable in the LB before the instance exists: HAProxy's own health check (1s)
			# decides when it gets traffic, as an orchestrator's readiness probe would.
			bench/cluster.sh lb "set server nimlet/n2 state ready" >/dev/null
			mark "$d" "timeout$timeout start 2 (cycle $cycle)"; bench/cluster.sh start 2
			sleep 30
			mark "$d" "timeout$timeout remove 2 (cycle $cycle)"; bench/cluster.sh remove 2
			sleep 15
		done
		wait "$lgpid"
		cp "$d/events.txt" "$d/timeout$timeout.events.txt"
	done
}

# 4. One failure per run at constant load on 4 instances.
faults() {
	d="$OUT/faults"
	mkdir -p "$d"
	for fault in ${FAULTS:-kill-instance pause-redis restart-redis restart-postgres boot-without-redis}; do
		fresh "$d" "$WL_SYNC"
		bench/cluster.sh up 4
		: > "$d/events.txt"
		# shellcheck disable=SC2086
		loadgen "$d" "$fault" $WL_SYNC -rate "$FAULT_RATE" -duration 120s -slo 60s &
		lgpid=$!
		sleep 40
		mark "$d" "$fault begin"
		case $fault in
		kill-instance) bench/cluster.sh kill 4 ;;
		pause-redis) "$RT" pause nimlet-bench-redis >/dev/null; sleep 10; "$RT" unpause nimlet-bench-redis >/dev/null ;;
		restart-redis) "$RT" restart -t 0 nimlet-bench-redis >/dev/null; bench/pin.sh nimlet-bench-redis "$REDIS_CPUS" ;;
		restart-postgres)
			"$RT" restart -t 5 nimlet-bench-pg >/dev/null
			bench/pin.sh nimlet-bench-pg "$PG_CPUS"
			;;
		boot-without-redis)
			# Does an instance that boots while Redis is unreachable refuse to start, or start
			# without its subscriptions (and then never wake its long-polls)?
			"$RT" pause nimlet-bench-redis >/dev/null
			(bench/cluster.sh start 5 > "$d/$fault.start5.txt" 2>&1 || true) &
			startpid=$!
			sleep 10
			"$RT" unpause nimlet-bench-redis >/dev/null
			wait "$startpid" || true
			"$RT" logs nimlet-bench-n5 > "$d/$fault.n5.log" 2>&1 || true
			;;
		esac
		mark "$d" "$fault end"
		wait "$lgpid"
		cp "$d/events.txt" "$d/$fault.events.txt"
	done
}

for e in $EXPERIMENTS; do
	echo "== $e"
	"$e"
done
bench/cluster.sh down
