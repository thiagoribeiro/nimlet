#!/bin/sh
# Pins every process (and thread) of a running container to a CPU list:
#   bench/pin.sh CONTAINER CPUS        e.g. bench/pin.sh nimlet-bench-pg 0-2
# Rootless podman doesn't delegate the cpuset controller, so --cpuset-cpus fails; plain
# sched_setaffinity on our own processes works, and later forks (Postgres backends) inherit it.
# Go reads the new affinity on its own (Go >= 1.25 re-evaluates GOMAXPROCS at runtime).
set -eu
RT=$(command -v docker || command -v podman)
pid=$("$RT" inspect -f '{{.State.Pid}}' "$1")
cg=$(awk -F: '$1=="0" {print $3}' "/proc/$pid/cgroup")
for p in $(cat "/sys/fs/cgroup$cg/cgroup.procs"); do
	taskset -a -cp "$2" "$p" >/dev/null 2>&1 || true
done
