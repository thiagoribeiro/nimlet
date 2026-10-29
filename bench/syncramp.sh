#!/bin/sh
# Memory under a growing number of concurrent /sync long-polls (docs/benchmarks.md, 07).
#   bench/syncramp.sh <results-dir-name>
# One instance (profile large, no quota) behind HAProxy; for each count in RAMP a loadgen run
# keeps that many listeners in a long-poll loop while senders trickle RATE msg/s spread over
# every room, then exits and the instance idles for GAP. The last counts ramp back down, to
# see whether memory is given back. bench/memsample.sh samples the whole run.
# Output: bench/results/<name>/{mem.csv,events.txt,l<count>-<i>.*}.
#
# Knobs (env): RAMP (1000 2000 4000 8000 16000 4000 1000), MEMBERS (16000, >= max of RAMP),
# ROOMS (MEMBERS/20), RATE (200), DURATION (60s), GAP (20s), PROFILE (large),
# CPU_PER_INSTANCE (empty: no quota), LG_CPUS (4,7).
set -eu
cd "$(dirname "$0")/.."
NAME=$1
OUT="bench/results/$NAME"
mkdir -p "$OUT"
RT=$(command -v docker || command -v podman)

export PG_PORT=${PG_PORT:-15432} REDIS_PORT=${REDIS_PORT:-16379}
export PG_CPUS=${PG_CPUS:-0-2} REDIS_CPUS=${REDIS_CPUS:-7} LB_CPUS=${LB_CPUS:-3} INST_CPUS=${INST_CPUS:-5-6}
export HOSTNET=1 NO_S3=1 PG_LOCKS=${PG_LOCKS:-256} PG_RAM=${PG_RAM-1}
export NIMLET_BENCH_PG_IMAGE=${NIMLET_BENCH_PG_IMAGE:-docker.io/library/postgres:18-alpine}
export CPU_PER_INSTANCE=${CPU_PER_INSTANCE:-}
export NIMLET_ENV="NIMLET_PROFILE=${PROFILE:-large}"
RAMP=${RAMP:-1000 2000 4000 8000 16000 4000 1000}
MEMBERS=${MEMBERS:-16000}
ROOMS=${ROOMS:-$((MEMBERS / 20))}
RATE=${RATE:-200}
DURATION=${DURATION:-60s}
GAP=${GAP:-20s}
LG_CPUS=${LG_CPUS:-4,7}
LB=http://127.0.0.1:18080
# Every member sends into its own room (member i is in room i%ROOMS), so with one sender per
# room each room gets RATE/ROOMS msg/s and every listener wakes now and then.
WL="-members $MEMBERS -rooms $ROOMS -senders $ROOMS"

LG=bin/loadgen
go build -o "$LG" ./bench/loadgen
bench/cluster.sh build
{
	echo "commit $(git rev-parse --short HEAD) $(date -Is)"
	echo "ramp: $RAMP members=$MEMBERS rooms=$ROOMS rate=$RATE duration=$DURATION profile=${PROFILE:-large}"
	uptime
	top -bn1 -o %CPU | head -20
} > "$OUT/host-$(date +%s).txt"
mark() { echo "$(date +%s) $*" >> "$OUT/events.txt"; }

# The state file holds access tokens, so it stays in bin/ (gitignored).
STATE=bin/bench-state.json
bench/cluster.sh down
bench/infra.sh up 2>/dev/null
bench/cluster.sh start 9 2>/dev/null
# shellcheck disable=SC2086
"$LG" -setup-addr http://127.0.0.1:18089 -addr "$LB" $WL -setup-concurrency 8 -setup-only -state "$STATE"
bench/cluster.sh stop 9
bench/cluster.sh up 1

bench/memsample.sh "$OUT/mem.csv" &
sampler=$!
trap 'kill $sampler 2>/dev/null || true' EXIT
mark idle
sleep "$GAP"
i=0
for n in $RAMP; do
	i=$((i + 1))
	base="l$n-$i"
	mark "start $base"
	# shellcheck disable=SC2086
	taskset -c "$LG_CPUS" "$LG" -addr "$LB" -state "$STATE" -out "$OUT/$base" $WL -listeners "$n" \
		-rate "$RATE" -duration "$DURATION" > "$OUT/$base.txt" 2>&1 || true
	mark "end $base"
	grep -E "^(listening|sends|deliveries|integrity):" "$OUT/$base.txt" | sed "s|^|  $base: |" || true
	sleep "$GAP"
done
mark done
