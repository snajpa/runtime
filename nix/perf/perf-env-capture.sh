#!/bin/sh
# perf-env-capture — host environment capture for the perf-regression suite (lane A).
# Emits a key: value block + a fingerprint for baseline keying (design §2/§3).
# run; emits a key: value block + a fingerprint for baseline keying (design §3/§5).
# v0.4: + reviewer1's mandatory covariates (2038/2057): kernel cmdline, CPU
# topology/NUMA, turbo, IRQ/RCU state, PSI memory/cpu/io, mem available, net state.
# v0.3: + per-side tool versions (fio/perf/bpftrace; lane C 1978: host fio 3.38 ≠ guest 3.36).
# v0.2: adds perf_event_paranoid + kptr_restrict (Lane E's manifest note).
# Usage: sh perf-env-capture.sh [artifacts-path]
set -u
FP_KEYS="uname_r kernel_config_sha256 cpu_model microcode cgroup_controllers cpu_topology_sha"
fp_in=""
out() {
  printf '%s: %s\n' "$1" "$2"
  case " $FP_KEYS " in *" $1 "*) fp_in="$fp_in$1=$2
";; esac
}
out captured_at_utc "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
out hostname "$(hostname)"
out uname_r "$(uname -r)"
out uname_v "$(uname -v)"
out kernel_cmdline "$(cat /proc/cmdline 2>/dev/null || echo n/a)"
cfg=""
[ -r "/boot/config-$(uname -r)" ] && cfg="/boot/config-$(uname -r)"
[ -z "$cfg" ] && [ -r /proc/config.gz ] && cfg=/proc/config.gz
if [ -n "$cfg" ]; then
  case "$cfg" in
    *.gz) h=$(zcat "$cfg" | sha256sum | cut -d' ' -f1) ;;
    *)    h=$(sha256sum "$cfg" | cut -d' ' -f1) ;;
  esac
  out kernel_config_source "$cfg"
  out kernel_config_sha256 "$h"
else
  out kernel_config_source none
  out kernel_config_sha256 n/a
fi
out cpu_model "$(grep -m1 'model name' /proc/cpuinfo | sed 's/^[^:]*: //')"
out cpu_count "$(nproc)"
out cpu_topology_sha "$(lscpu -e=CPU,CORE,SOCKET,NODE 2>/dev/null | sha256sum | cut -d' ' -f1)"
out numa_nodes "$(lscpu 2>/dev/null | awk -F: '/NUMA node\(s\)/{gsub(/ /,"",$2); print $2}')"
out microcode "$(grep -m1 '^microcode' /proc/cpuinfo | awk '{print $3}')"
for f in /sys/devices/system/cpu/vulnerabilities/*; do
  [ -r "$f" ] && out "mitigation_$(basename "$f")" "$(cat "$f")"
done
out cgroup_fs "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)"
out cgroup_controllers "$(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null)"
out perf_event_paranoid "$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null)"
out kptr_restrict "$(cat /proc/sys/kernel/kptr_restrict 2>/dev/null)"
out rcu_normal "$(cat /sys/kernel/rcu_normal 2>/dev/null || echo n/a)"
out rcu_expedited "$(cat /sys/kernel/rcu_expedited 2>/dev/null || echo n/a)"
out irqbalance "$(command -v irqbalance >/dev/null 2>&1 && { pgrep -x irqbalance >/dev/null 2>&1 && echo running || echo present-not-running; } || echo absent)"
v=$(fio --version 2>/dev/null | head -1); [ -n "$v" ] || v=absent; out fio_version "$v"
v=$(perf --version 2>/dev/null | head -1); [ -n "$v" ] || v=absent; out perf_version "$v"
v=$(bpftrace --version 2>/dev/null | head -1); [ -n "$v" ] || v=absent; out bpftrace_version "$v"
out ulimit_nofile "$(ulimit -n)"
out ulimit_memlock "$(ulimit -l 2>/dev/null)"
out mem_total_kb "$(awk '/MemTotal/{print $2}' /proc/meminfo)"
out mem_available_kb "$(awk '/MemAvailable/{print $2}' /proc/meminfo)"
out swap_total_kb "$(awk '/SwapTotal/{print $2}' /proc/meminfo)"
out psi_cpu "$(head -1 /proc/pressure/cpu 2>/dev/null || echo n/a)"
out psi_memory "$(head -1 /proc/pressure/memory 2>/dev/null || echo n/a)"
out psi_io "$(head -1 /proc/pressure/io 2>/dev/null || echo n/a)"
if [ -d /sys/devices/system/cpu/cpu0/cpufreq ]; then
  out governor "$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null)"
else
  out governor "n/a (no cpufreq sysfs)"
fi
if [ -d /sys/devices/system/cpu/intel_pstate ]; then
  out turbo "$(cat /sys/devices/system/cpu/intel_pstate/no_turbo 2>/dev/null || echo n/a)"
else
  out turbo "n/a (no intel_pstate)"
fi
if [ -n "${1:-}" ]; then
  out artifacts_path "$1"
  out artifacts_fs "$(stat -fc %T "$1" 2>/dev/null || echo missing)"
fi
out loadavg "$(cat /proc/loadavg)"
ni=$(ip -brief link 2>/dev/null | awk '{print $1":"$2}' | tr '\n' ' '); [ -n "$ni" ] || ni=n/a
out net_ifaces "$ni"
echo "disks:"
lsblk -o NAME,SIZE,ROTA,TYPE,MOUNTPOINTS 2>/dev/null | sed 's/^/  /'
printf 'fingerprint_sha256: %s\n' "$(printf '%b' "$fp_in" | sha256sum | cut -d' ' -f1)"
