#!/bin/sh
# Phase 05: write-path variants A/B against the phase-04 protocol (docs/benchmarks.md).
#   [VARIANTS='A B'] bench/batching.sh [ram] [disk] [profile]   (default: all three)
# Variants (instance env on top of the profile):
#   A  baseline: split protocol, profile batch limits
#   B  bigger batches: 100 sends or 20ms
#   C  single transaction holding the allocation lock until the commit
#   D  C + B
#   E  B on the adaptive queue (waits only after a group of more than one; needs that commit)
# ram:     sizes (sync-heavy) + scale send-heavy N=1,2,4,8, Postgres in /dev/shm, per variant
# disk:    scale send-heavy N=4, Postgres on disk, variants A C D
# profile: pg_stat_statements at 4000 msg/s on 8 small instances, per variant
# Output: bench/results/05-batching/<part>-<variant>/...
set -eu
cd "$(dirname "$0")/.."
PARTS=${*:-ram disk profile}

variant() {
	case $1 in
	A) echo "" ;;
	B | E) echo "NIMLET_EVENT_BATCH_MAX=100 NIMLET_EVENT_BATCH_WAIT_US=20000" ;;
	C) echo "NIMLET_EVENT_BATCH_MODE=single" ;;
	D) echo "NIMLET_EVENT_BATCH_MODE=single NIMLET_EVENT_BATCH_MAX=100 NIMLET_EVENT_BATCH_WAIT_US=20000" ;;
	esac
}

for part in $PARTS; do
	case $part in
	ram)
		for v in ${VARIANTS:-A B C D}; do
			VARIANT_ENV=$(variant $v) SCALE_WORKLOADS=send bench/horizontal.sh "05-batching/ram-$v" sizes scale
		done
		;;
	disk)
		for v in ${VARIANTS:-A C D}; do
			PG_RAM='' VARIANT_ENV=$(variant $v) SCALE_WORKLOADS=send SCALE_NS=4 bench/horizontal.sh "05-batching/disk-$v" scale
		done
		;;
	profile)
		for v in ${VARIANTS:-A B C D}; do
			NIMLET_ENV="NIMLET_PROFILE=small $(variant $v)" bench/pgprofile.sh "bench/results/05-batching/profile-$v"
		done
		;;
	*) echo "usage: $0 [ram] [disk] [profile]" >&2; exit 2 ;;
	esac
done
