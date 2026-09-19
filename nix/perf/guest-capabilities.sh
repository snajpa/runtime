#!/bin/sh
# Lane E — guest-side capability + covariate probe (R23).
# Runs on the host, ssh into the dev-VM; emits JSONL records aligned to the
# sealed schema: every guest capability carries {scope:guest, side, leg,
# capture_phase, name, requirement, status, substitute?...}; the terminal
# covariate record is a guest-scoped optional capability `guest.env` (guest
# kernel / memory / module / device only). No env record, no new record types,
# no host facts, no measurement.
set -u
PORT=${E2B_VM_PORT:-2225}
export SSHPASS=${E2B_VM_PASSWORD:-e2b-dev}
# W6 provenance context (A passes these explicitly; reviewer1 3241/3253) — fail fast if absent
side=${PERF_SIDE:-}; leg=${PERF_LEG:-}; cphase=${PERF_W6_CAPTURE_PHASE:-}
case "$side" in baseline|candidate) ;; *) echo "guest-capabilities: PERF_SIDE must be baseline|candidate (got '${PERF_SIDE:-}')" >&2; exit 2 ;; esac
case "$leg" in A|B|AA|A1|A2|B1|B2) ;; *) echo "guest-capabilities: PERF_LEG must be A|B|AA|A1|A2|B1|B2 (got '${PERF_LEG:-}')" >&2; exit 2 ;; esac
case "$cphase" in cold|warm) ;; *) echo "guest-capabilities: PERF_W6_CAPTURE_PHASE must be cold|warm (got '${PERF_W6_CAPTURE_PHASE:-}')" >&2; exit 2 ;; esac
sg() { sshpass -e ssh -p "$PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=10 dev@127.0.0.1 "$1"; }

ver_perf=$(sg 'perf --version 2>/dev/null' || true)
kframes=$(sg 'sudo perf record -F 99 -g -a -o /tmp/cap-perf.$$ -- sleep 1 >/dev/null 2>&1; sudo perf report --stdio -i /tmp/cap-perf.$$ 2>/dev/null | grep -m1 -c "kernel.kallsyms"; sudo rm -f /tmp/cap-perf.$$; true' || echo 0)
ver_fio=$(sg 'fio --version 2>/dev/null' || true)
ver_bpftrace=$(sg 'bpftrace --version 2>/dev/null | head -1' || true)
ublk_syms=$(sg 'sudo grep -c " [tT] ublk" /proc/kallsyms 2>/dev/null' || echo 0)
psi=$(sg 'test -e /proc/pressure/cpu && echo available || echo unavailable' || echo unavailable)
fg=$(sg 'command -v flamegraph.pl >/dev/null && echo available || echo unavailable' || echo unavailable)
kver=$(sg 'uname -r' || true)
mem=$(sg 'grep -m1 MemAvailable /proc/meminfo | awk "{print \$2}"' || true)
mods=$(sg 'lsmod | grep -E "ublk|nbd" | awk "{print \$1}" | tr "\n" "," ' || true)
devs=$(sg 'test -c /dev/ublk-control && echo ublk-control,; test -c /dev/kvm && echo kvm,' | tr '\n' ',' || true)

echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"perf.guest\",\"requirement\":\"to_execute\",\"status\":\"available\",\"version\":\"${ver_perf:-unknown}\",\"kernel_frames\":${kframes:-0}}"
echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"fio.guest\",\"requirement\":\"to_execute\",\"status\":\"available\",\"version\":\"${ver_fio:-unknown}\"}"
echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"bpftrace.guest\",\"requirement\":\"validity\",\"status\":\"available\",\"version\":\"${ver_bpftrace:-unknown}\"}"
echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"tracepoint.ublk\",\"requirement\":\"validity\",\"status\":\"unavailable\",\"substitute\":{\"for\":\"tracepoint.ublk\",\"via\":\"bpftrace-kprobe\",\"symbols\":${ublk_syms:-0}}}"
echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"flamegraph.guest\",\"requirement\":\"validity\",\"status\":\"${fg}\",\"substitute\":{\"for\":\"flamegraph.guest\",\"via\":\"store-copy-injection\"}}"
echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"psi.guest\",\"requirement\":\"optional\",\"status\":\"${psi}\"}"
echo "{\"record\":\"capability\",\"scope\":\"guest\",\"side\":\"${side}\",\"leg\":\"${leg}\",\"capture_phase\":\"${cphase}\",\"name\":\"guest.env\",\"requirement\":\"optional\",\"status\":\"available\",\"kernel\":\"${kver}\",\"mem_available_kb\":${mem:-0},\"modules\":\"${mods}\",\"devices\":\"${devs}\"}"
