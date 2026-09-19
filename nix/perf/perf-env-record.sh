#!/bin/sh
# perf-env-record — emit the perf/2 `env` record for a run (lane A).
# Companion of perf-env-capture.sh: supplies the env covariates for BOTH classes,
# including the quiet host-load fields the comparator needs (lane E 2210; the busy
# record additionally carries busy_load, emitted by the suite's busy hooks).
# Usage: sh perf-env-record.sh <quiet|busy> <run-dir>
# Prints ONE JSON line on stdout, ready for: emit.sh append --kind env --phase run --mode clean
set -u
class=${1:?usage: perf-env-record.sh <quiet|busy> <run-dir>}
rundir=${2:?usage: perf-env-record.sh <quiet|busy> <run-dir>}
self_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
c=$("$self_dir/perf-env-capture.sh" "$rundir" 2>/dev/null)
get() { printf '%s\n' "$c" | sed -n "s/^$1: //p" | head -1; }
fp=$(get fingerprint_sha256); [ -n "$fp" ] || fp=$( { cat /proc/cmdline /proc/cpuinfo; } 2>/dev/null | sha256sum | cut -d' ' -f1)
free_gb=$(df -Pk -- "$rundir" 2>/dev/null | awk 'NR==2 {printf "%.1f", $4/1048576}'); [ -n "$free_gb" ] || free_gb=0.0
fs=$(stat -fc %T "$rundir" 2>/dev/null || echo unknown)
tmp=false; [ "$fs" = tmpfs ] && tmp=true
python3 - "$class" "$fp" "$free_gb" "$tmp" "$rundir" "$c" <<'PY'
import json, sys
cls, fp, free, tmp, rundir, cap = sys.argv[1:7]
def g(k):
    for line in cap.splitlines():
        if line.startswith(k + ": "):
            return line.split(": ", 1)[1]
    return ""
def gi(k):
    try: return int(g(k))
    except ValueError: return 0
rec = {"record": "env", "env_class": cls,
       "host": {"fingerprint": fp,
                "uname_r": g("uname_r"),
                "cpu_count": gi("cpu_count"),
                "mem_available_kb": gi("mem_available_kb"),
                "loadavg": g("loadavg"),
                "psi": {"cpu": g("psi_cpu"), "memory": g("psi_memory"), "io": g("psi_io")}},
       "tools": {"fio": g("fio_version"), "perf": g("perf_version"), "bpftrace": g("bpftrace_version")},
       "disk_guard": {"path": rundir, "free_gb": float(free), "floor_gb": 40, "tmpfs": tmp == "true"}}
print(json.dumps(rec))
PY
