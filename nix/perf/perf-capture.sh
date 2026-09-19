#!/bin/sh
# perf-capture — unit runner for the perf-regression suite (lane C).
# Modes:
#   perf-capture.sh capabilities
#   perf-capture.sh run <clean|trace|diagnostic> <unit-id> <artifacts-dir> [--lock] -- <cmd...>
#   perf-capture.sh flamegraph <unit-id> <artifacts-dir>
#
# Output: perf/2 T1 *bodies* on stdout, one JSON object per line — feed each to
# `schema/emit.sh append --run-dir <dir> --kind <record>` (the emitter adds the
# envelope). Bodies: capability records; a `note` carrying the `lock` object
# when the device window is held; a `note` unit summary; `artifact` records
# (sha256, retain:true).
#
# Contract (design §5/§6): artifacts dirs are persistent (never tmpfs);
# diagnostic perf.data is retained in full; clean mode attaches no profiler
# (trace/diagnostic are post-confirmation modes); the device lock resolves
# exactly like the test helper and the wait is recorded; per-unit stdout/stderr
# are retained.
#
# Env knobs: FGRAPH (FlameGraph bin dir), TRACE_EVENTS, PERF_FREQ, CALLGRAPH.

set -eu

if [ -z "${FGRAPH:-}" ]; then
	if command -v flamegraph.pl >/dev/null 2>&1; then
		FGRAPH=$(dirname "$(command -v flamegraph.pl)")
	else
		FGRAPH=/nix/store/b5dzn0k3fgylih6ac0zibp8hpcwffk2f-FlameGraph-2023-11-06/bin
	fi
fi
TRACE_EVENTS="${TRACE_EVENTS:-block:block_rq_issue,block:block_rq_complete,io_uring:io_uring_submit_req,nbd:nbd_send_request}"
PERF_FREQ="${PERF_FREQ:-997}"
CALLGRAPH="${CALLGRAPH:-fp}"
SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

lock_path() {
	if [ -n "${E2B_NBD_TEST_LOCK:-}" ]; then
		printf '%s' "$E2B_NBD_TEST_LOCK"
	else
		printf '%s/e2b-nbd-device-tests.lock' "${TMPDIR:-/tmp}"
	fi
}

sha256_of() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

jcap() { # name requirement status [substitute_for substitute_via]
	printf '{"record":"capability","scope":"host","name":"%s","requirement":"%s","status":"%s"' "$1" "$2" "$3"
	if [ "${4:-}" != "" ]; then
		printf ',"substitute":{"for":"%s","via":"%s"}' "$4" "$5"
	fi
	printf '}\n'
}

capabilities() {
	if perf --version >/dev/null 2>&1; then
		jcap perf to_execute available
	else
		jcap perf to_execute unavailable
	fi
	if command -v bpftrace >/dev/null 2>&1 && [ -r /sys/kernel/btf/vmlinux ]; then
		jcap bpftrace optional available
	else
		jcap bpftrace optional unavailable
	fi
	if [ -x "$FGRAPH/flamegraph.pl" ] && [ -x "$FGRAPH/stackcollapse-perf.pl" ]; then
		jcap flamegraph optional available
	else
		jcap flamegraph optional unavailable
	fi
	perflist=$(perf list 2>/dev/null || true)
	for g in block io_uring nbd; do
		n=$(printf '%s\n' "$perflist" | grep -cE "^  ${g}:" || true)
		if [ "${n:-0}" -gt 0 ] 2>/dev/null; then
			printf '{"record":"capability","scope":"host","name":"tracepoints:%s","requirement":"optional","status":"available","count":%s}\n' "$g" "$n"
		else
			printf '{"record":"capability","scope":"host","name":"tracepoints:%s","requirement":"optional","status":"unavailable","count":0}\n' "$g"
		fi
	done
	if lsmod 2>/dev/null | grep -q '^ublk_drv'; then
		n=$(bpftrace -l 'kprobe:ublk*' 2>/dev/null | wc -l)
		printf '{"record":"capability","scope":"host","name":"capture:ublk","requirement":"optional","status":"available","count":%s,"substitute":{"for":"tracepoints:ublk","via":"bpftrace-kprobe/kfunc"}}\n' "$n"
	else
		printf '{"record":"capability","scope":"host","name":"capture:ublk","requirement":"optional","status":"unavailable","count":0,"substitute":{"for":"tracepoints:ublk","via":"bpftrace-kprobe/kfunc (modprobe ublk_drv first)"}}\n'
	fi
	if command -v fio >/dev/null 2>&1; then
		jcap fio to_execute available
	else
		jcap fio to_execute unavailable
	fi
	if command -v python3 >/dev/null 2>&1; then
		jcap python3 to_execute available
	else
		jcap python3 to_execute unavailable
	fi
	lp=$(lock_path)
	if ( : >>"$lp" ) 2>/dev/null; then
		printf '{"record":"capability","scope":"host","name":"device-lock","requirement":"to_execute","status":"available","path":"%s"}\n' "$lp"
	else
		printf '{"record":"capability","scope":"host","name":"device-lock","requirement":"to_execute","status":"permission-denied","path":"%s"}\n' "$lp"
	fi
}

artifact() { # unit kind path
	printf '{"record":"artifact","unit":"%s","kind":"%s","path":"%s","sha256":"%s","retain":true}\n' "$1" "$2" "$3" "$(sha256_of "$3")"
}

make_flamegraph() { # unit adir - regenerate script/folded/SVG from retained perf.data
	unit=$1
	adir=$2
	if [ ! -f "$adir/$unit.perf.data" ]; then
		echo "no retained perf.data: $adir/$unit.perf.data" >&2
		return 2
	fi
	perf script -i "$adir/$unit.perf.data" >"$adir/$unit.perf.script" 2>/dev/null || true
	"$FGRAPH/stackcollapse-perf.pl" "$adir/$unit.perf.script" >"$adir/$unit.folded" 2>/dev/null || true
	"$FGRAPH/flamegraph.pl" "$adir/$unit.folded" >"$adir/$unit.svg" 2>/dev/null || true
	artifact "$unit" flamegraph "$adir/$unit.svg"
}

run() {
	mode=$1
	unit=$2
	adir=$3
	shift 3
	use_lock=0
	if [ "${1:-}" = "--lock" ]; then
		use_lock=1
		shift
	fi
	if [ "${1:-}" = "--" ]; then
		shift
	fi
	mkdir -p "$adir/logs"

	if [ "$use_lock" = 1 ]; then
		lp=$(lock_path)
		exec 9>>"$lp"
		t0=$(date +%s.%N)
		flock -w 1200 9
		t1=$(date +%s.%N)
		wait_s=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", b-a}')
		printf '{"record":"note","text":"device lock held for this unit","lock":{"path":"%s","scope":"device-window","wait_s":%s,"acquired":true}}\n' "$lp" "$wait_s"
	fi

	case "$mode" in
	clean)
		python3 "$SELF_DIR/perf-rusage.py" "$adir/logs/$unit" -- "$@"
		;;
	trace)
		LOG_STDOUT="$adir/logs/$unit.stdout" LOG_STDERR="$adir/logs/$unit.stderr" \
			perf record -o "$adir/$unit.trace.data" -e "$TRACE_EVENTS" -a -- \
				sh -c 'exec "$@" >"$LOG_STDOUT" 2>"$LOG_STDERR"' sh "$@"
		artifact "$unit" perf-trace "$adir/$unit.trace.data"
		;;
	diagnostic)
		LOG_STDOUT="$adir/logs/$unit.stdout" LOG_STDERR="$adir/logs/$unit.stderr" \
			perf record -o "$adir/$unit.perf.data" -F "$PERF_FREQ" -g --call-graph "$CALLGRAPH" -a -- \
				sh -c 'exec "$@" >"$LOG_STDOUT" 2>"$LOG_STDERR"' sh "$@"
		artifact "$unit" perf-data "$adir/$unit.perf.data"
		make_flamegraph "$unit" "$adir"
		;;
	*)
		echo "unknown mode: $mode" >&2
		exit 2
		;;
	esac
}

cmd=${1:-}
if [ "$cmd" = flamegraph ]; then
	make_flamegraph "$@"
	exit $?
fi
shift || true
case "$cmd" in
capabilities)
	capabilities
	;;
run)
	run "$@"
	;;
*)
	echo "usage: $0 capabilities | run <clean|trace|diagnostic> <unit> <dir> [--lock] -- <cmd...> | flamegraph <unit> <dir>" >&2
	exit 2
	;;
esac
