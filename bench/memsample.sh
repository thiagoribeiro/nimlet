#!/bin/sh
# Memory of a bench/cluster.sh run, sampled every 2s until killed (docs/benchmarks.md, 07).
#   bench/memsample.sh OUT.csv &
# Per nimlet-bench-* container: cgroup memory.current and memory.stat anon/file/shmem. Per
# instance (1..8): Go heap in use, Go sys and goroutines from its /metrics. Also the loadgen's
# RSS and, every 10s, the size of the Postgres data dir when PG_RAM keeps it in /dev/shm
# (it is shmem charged to the Postgres cgroup, so subtract it).
# Columns: ts,name,current,anon,file,shmem,heap_inuse,go_sys,goroutines (bytes).
set -u
OUT=$1
RT=$(command -v docker || command -v podman)
PG_RAM_DIR=/dev/shm/nimlet-bench-pg
echo "ts,name,current,anon,file,shmem,heap_inuse,go_sys,goroutines" > "$OUT"
i=0
while :; do
	ts=$(date +%s)
	for id in $("$RT" ps -q --filter name=nimlet-bench 2>/dev/null); do
		set -- $("$RT" inspect -f '{{.Name}} {{.State.CgroupPath}}' "$id" 2>/dev/null)
		name=${1#/} cg=/sys/fs/cgroup${2:-}
		[ -r "$cg/memory.current" ] || continue
		cur=$(cat "$cg/memory.current")
		mem=$(awk '$1=="anon"{a=$2} $1=="file"{f=$2} $1=="shmem"{s=$2} END{print a+0 "," f+0 "," s+0}' "$cg/memory.stat")
		heap="" sys="" gor=""
		case $name in nimlet-bench-n[1-8])
			m=$(curl -s --max-time 1 "http://127.0.0.1:1818${name#nimlet-bench-n}/metrics")
			heap=$(echo "$m" | awk '$1=="go_memstats_heap_inuse_bytes"{print $2}')
			sys=$(echo "$m" | awk '$1=="go_memstats_sys_bytes"{print $2}')
			gor=$(echo "$m" | awk '$1=="go_goroutines"{print $2}')
			;;
		esac
		echo "$ts,$name,$cur,$mem,$heap,$sys,$gor" >> "$OUT"
	done
	lg=$(pgrep -x loadgen | head -1)
	[ -n "$lg" ] && echo "$ts,loadgen,$(awk '/VmRSS/{print $2*1024}' "/proc/$lg/status" 2>/dev/null),,,,,," >> "$OUT"
	if [ $((i % 5)) -eq 0 ] && [ -d "$PG_RAM_DIR" ]; then
		if [ "$(basename "$RT")" = podman ]; then du=$(podman unshare du -sb "$PG_RAM_DIR"); else du=$(du -sb "$PG_RAM_DIR"); fi
		echo "$ts,pg-datadir,$(echo "$du" | cut -f1),,,,,," >> "$OUT"
	fi
	i=$((i + 1))
	sleep 2
done
