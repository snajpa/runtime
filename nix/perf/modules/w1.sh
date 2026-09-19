#!/bin/sh
# W1 module — ublk device benches (perf-regression suite).
# Scaffold by agent0 (2026-09-19); owner lane D (runner semantics/cell grid),
# lane C (capture/device-window region — landed). Contract (nix/perf/README.md):
#   list                         -> profile ids (one per line)
#   oracle <profile> [outdir]    -> step-0 correctness gate (in-VM fio verify fixture)
#   run <profile> <leg> <block> <leg-dir>
#                                -> one measured block; raw records appended to
#                                   <leg-dir>/samples.jsonl; artifacts in <leg-dir>; no verdicts
#   profile <profile> <leg-dir>  -> matched-diagnostics capture (never gating)
#   build                        -> build the bench test binary (host) and print its path
# Env: PERF_ENV_CLASS, PERF_LEG, PERF_SIDE, PERF_STAGE, PERF_TREE_DIR / PERF_TREES_DIR,
#      PERF_RUN_ID/PERF_PHASE/PERF_MODE/PERF_SUITE_*, PERF_CAPTURE, PERF_W1_DRY,
#      PERF_W1_VM_PORT (default E2B_VM_PORT, then 2222), E2B_VM_HOST /
#      E2B_VM_PASSWORD, PERF_W1_VM_STATE (dev.sh state dir), PERF_W1_ENSURE_VM,
#      PERF_W1_VM_BIN (E2B_DEV_VM_BIN override), PERF_W1_CACHE, PERF_W1_RUNTIME
#      (s), PERF_W1_SIZE_MIB, PERF_W1_ORACLE_MIB, PERF_W1_TIMEOUT (s),
#      PERF_W1_FIO, PERF_W1_KEEP (keep VM-side artifacts), PERF_W1_PROFILE_CMD.
# Exit: 0 ok / 30 inconclusive / 40 setup_error (suite scheme).
#
# Measured flow (lane D): the host builds the tree's ublk test binary with a
# `go test -c -overlay` driver (nix/perf/modules/w1/bench/w1_bench_test.go — the
# tree stays untouched), ships it into the dev VM, runs one io_uring fio job per
# profile against the fileBackend fixture (2 queues, 1 MiB max I/O), collects
# fio.json + meta.json, and emits perf/2 samples
# (nix/perf/modules/w1/emit_samples.py). Default usage is envelope mode
# (candidate-only; no_historical_baseline) — all legs run the candidate tree;
# paired within-ublk `--baseline` runs (fresh before/after, ABBA-interleaved)
# are supported, while the historical NBD↔NBD comparison stays W2's path.
# The device window (lane C's shared lock) is held
# across the VM device jobs (oracle fixture + measured run), not the host-side
# build/ship.
set -u

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
TREES=${PERF_TREES_DIR:-/root/ai/worktrees/e2b}
DRY=${PERF_W1_DRY:-0}
CLASS=${PERF_ENV_CLASS:-quiet}
CAPTURE=${PERF_CAPTURE:-$HERE/../perf-capture.sh}
[ -x "$CAPTURE" ] || CAPTURE=""
LOCK_PATH=${E2B_NBD_TEST_LOCK:-${TMPDIR:-/tmp}/e2b-nbd-device-tests.lock}

# lane D knobs: VM access, bench build cache, job shape.
VM_PORT=${PERF_W1_VM_PORT:-${E2B_VM_PORT:-2222}}
VM_HOST=${E2B_VM_HOST:-dev@127.0.0.1}
VM_PASS=${E2B_VM_PASSWORD:-e2b-dev}
VM_STATE=${PERF_W1_VM_STATE:-$HOME/ai/logs/e2b-perf/w1-vm-state}
ENSURE_VM=${PERF_W1_ENSURE_VM:-1}
CACHE=${PERF_W1_CACHE:-$HOME/ai/logs/e2b-perf/w1-cache}
RUNTIME=${PERF_W1_RUNTIME:-30}
SIZE_MIB=${PERF_W1_SIZE_MIB:-1024}
ORACLE_MIB=${PERF_W1_ORACLE_MIB:-64}
TIMEOUT=${PERF_W1_TIMEOUT:-900}
FIO_BIN=${PERF_W1_FIO:-fio}
KEEP=${PERF_W1_KEEP:-0}
BENCH_SRC=$HERE/w1/bench/w1_bench_test.go
EMIT=$HERE/w1/emit_samples.py

say() { echo "+ $*" >&2; }
fail() { echo "w1: $*" >&2; exit 40; }

# profile -> "<rw> <bs> <extra fio args>" ; keep ids stable for the registry.
profile_params() {
	case "$1" in
	seqread-4k)          echo "read      4k   -" ;;
	seqread-128k)        echo "read      128k -" ;;
	seqwrite-4k)         echo "write     4k   -" ;;
	seqwrite-128k)       echo "write     128k -" ;;
	randread-4k)         echo "randread  4k   -" ;;
	randread-128k)       echo "randread  128k -" ;;
	randwrite-4k)        echo "randwrite 4k   -" ;;
	randwrite-128k)      echo "randwrite 128k -" ;;
	randwrite-modify-4k) echo "randwrite 4k   --prefill" ;;
	randrw-50-4k)        echo "randrw    4k   --rwmixread=50" ;;
	randrw-50-128k)      echo "randrw    128k --rwmixread=50" ;;
	randrw-70-4k)        echo "randrw    4k   --rwmixread=70" ;;
	randrw-70-128k)      echo "randrw    128k --rwmixread=70" ;;
	trim-4k)             echo "trim      4k   -" ;;
	write-fsync-4k)      echo "write     4k   --fsync=1" ;;
	randread-4k-qd1)     echo "randread  4k   --iodepth=1" ;;
	randread-4k-qd32)    echo "randread  4k   --iodepth=32" ;;
	*) return 1 ;;
	esac
}

resolve_tree() {
	case "${PERF_LEG:-candidate}" in
	A*) leg_side=baseline ;;
	B*) leg_side=candidate ;;
	*) leg_side=${PERF_LEG:-candidate} ;;
	esac
	tree=${PERF_TREE_DIR:-"$TREES/perf-$leg_side"}
	[ -d "$tree" ] || fail "no tree dir $tree (set PERF_TREE_DIR or materialize it)"
	echo "$tree"
}

prereq_checks() {
	tree=$(resolve_tree)
	[ -d "$tree/packages/orchestrator/pkg/sandbox/ublk" ] || \
		fail "candidate tree lacks pkg/sandbox/ublk ($tree)"
	command -v flock >/dev/null 2>&1 || fail "flock(1) missing (device-window lock)"
	command -v python3 >/dev/null 2>&1 || fail "python3 missing (sample emitter)"
	command -v tar >/dev/null 2>&1 || fail "tar missing (artifact collection)"
	echo "$tree"
}

device_lock_acquire() { # lane C region: per-cell hold of the shared device window
	# One rendezvous across suite/gate/dev invocations — a divergent
	# E2B_NBD_TEST_LOCK path is a disjoint lock (no exclusion); the path actually
	# used is recorded. Budget <=20 min; expiry is a safe refusal
	# (SETUP_ERROR), never FAIL. One device job at a time; the fd closes at exit
	# (one cycle per invocation) = the conservative per-cell hold.
	out=${1:-}
	exec 9>>"$LOCK_PATH" || fail "cannot open device lock: $LOCK_PATH"
	t0=$(date +%s.%N)
	if ! flock -w 1200 9; then
		say "device window busy >20 min at $LOCK_PATH — safe refusal (SETUP_ERROR, never FAIL)"
		exit 40
	fi
	wait_s=$(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.3f", b-a}')
	DEVICE_LOCK_HELD=1
	DEVICE_LOCK_WAIT_S=$wait_s
	say "device window held: $LOCK_PATH (waited ${wait_s}s)"
	if [ -n "$out" ]; then
		mkdir -p "$out"
		printf '{"record":"note","text":"device lock held for this cell","lock":{"path":"%s","scope":"device-window","wait_s":%s,"acquired":true}}\n' "$LOCK_PATH" "$wait_s" >>"$out/samples.jsonl"
	fi
}

device_lock_release() { # lane C region: release the device window before process exit
	# Pair with device_lock_acquire(); a no-op when nothing is held. Modules may
	# call this right after the measured VM job so the window doesn't extend
	# through collect/emit; otherwise it simply releases at process exit.
	[ -n "${DEVICE_LOCK_HELD:-}" ] || return 0
	flock -u 9 2>/dev/null || true
	exec 9>&- || true
	DEVICE_LOCK_HELD=""
	say "device window released (waited ${DEVICE_LOCK_WAIT_S:-0}s at acquire)"
}

# --- VM plumbing (lane D) ---------------------------------------------------

vm_ssh() {
	SSHPASS=$VM_PASS sshpass -e ssh -p "$VM_PORT" \
		-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null \
		-o LogLevel=ERROR -o ConnectTimeout=10 "$VM_HOST" "$@"
}

vm_scp() {
	SSHPASS=$VM_PASS sshpass -e scp -P "$VM_PORT" \
		-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null \
		-o LogLevel=ERROR "$@"
}

vm_up() { vm_ssh true >/dev/null 2>&1; }

ensure_vm() { # $1 = tree (for dev.sh); 0 = reachable
	vm_up && return 0
	[ "$ENSURE_VM" = 1 ] || return 1
	command -v sshpass >/dev/null 2>&1 || fail "sshpass missing (VM access)"
	say "VM down on $VM_HOST:$VM_PORT — dev.sh --ensure (state: $VM_STATE)"
	mkdir -p "$CACHE" 2>/dev/null || true
	(
		cd "$1" || exit 1
		export E2B_DEV_VM_DIR="$VM_STATE" E2B_DEV_VM_SSH_PORT="$VM_PORT"
		export E2B_VM_PORT="$VM_PORT" E2B_VM_HOST="$VM_HOST" E2B_VM_PASSWORD="$VM_PASS"
		[ -z "${PERF_W1_VM_BIN:-}" ] || export E2B_DEV_VM_BIN="$PERF_W1_VM_BIN"
		sh nix/scripts/dev.sh --ensure
	) >"$CACHE/vm-ensure.log" 2>&1 || true
	# dev.sh can bail early (its ssh-grace loop) while QEMU keeps booting; wait
	# for the VM itself before refusing. PERF_W1_VM_WAIT is in 5s ticks (36=180s).
	i=0
	while [ "$i" -lt "${PERF_W1_VM_WAIT:-36}" ]; do
		vm_up && return 0
		i=$((i + 1))
		sleep 5
	done
	vm_up
}

vm_prereqs() { # ublk control device + fio inside the VM
	vm_ssh "sudo -n modprobe ublk_drv 2>/dev/null; test -c /dev/ublk-control && command -v '$FIO_BIN' >/dev/null && echo w1-prereqs-ok" 2>/dev/null | grep -q w1-prereqs-ok
}

build_bench() { # $1 = tree -> bench test binary path on stdout
	tree=$(cd -- "$1" && pwd) || fail "tree not a directory: $1"
	[ -f "$BENCH_SRC" ] || fail "bench driver missing: $BENCH_SRC"
	command -v go >/dev/null 2>&1 || fail "go toolchain missing (host bench build)"
	command -v sha256sum >/dev/null 2>&1 || fail "sha256sum missing"
	sha=$(sha256sum "$BENCH_SRC" | cut -d' ' -f1)
	head=$(git -C "$tree" rev-parse HEAD 2>/dev/null || echo nogit)
	head12=$(printf '%.12s' "$head")
	gov=$(go version 2>/dev/null | sha256sum | cut -d' ' -f1)
	key=$(printf '%s %s %s' "$head" "$sha" "$gov" | sha256sum | cut -d' ' -f1 | cut -c1-16)
	bin="$CACHE/ublk-bench-$key.test"
	if [ -x "$bin" ]; then echo "$bin"; return 0; fi
	mkdir -p "$CACHE" || fail "cannot create bench cache $CACHE"
	overlay="$CACHE/overlay-$key.json"
	printf '{"Replace":{"%s/packages/orchestrator/pkg/sandbox/ublk/w1_bench_test.go":"%s"}}\n' "$tree" "$BENCH_SRC" >"$overlay" || fail "overlay write failed"
	log="$CACHE/build-$key.log"
	say "bench: building ublk bench from $tree (head $head12, log $log)"
	if ! (cd "$tree/packages/orchestrator" && CGO_ENABLED=0 go test -c -overlay "$overlay" -o "$bin.tmp" ./pkg/sandbox/ublk/) >"$log" 2>&1; then
		echo "w1: bench build failed (log: $log)" >&2
		tail -n 20 "$log" >&2 || true
		rm -f "$bin.tmp"
		return 1
	fi
	mv "$bin.tmp" "$bin" || return 1
	echo "$bin"
}

ship_bench() { # $1 = local bench binary -> remote path on stdout
	bin=$1
	rbin="/tmp/$(basename "$bin")"
	if vm_ssh "test -x '$rbin'"; then echo "$rbin"; return 0; fi
	say "bench: shipping $(basename "$bin") to the VM ($rbin)"
	vm_scp "$bin" "$VM_HOST:$rbin" || return 1
	vm_ssh "chmod +x '$rbin'" >/dev/null 2>&1 || true
	echo "$rbin"
}

collect_dir() { # $1 = remote dir $2 = local dir; images (backend files) excluded
	mkdir -p "$2" || return 1
	if ! vm_ssh "tar -C '$1' --exclude='*.img' -cf - ." 2>/dev/null | tar -C "$2" -xf - 2>/dev/null; then
		say "collect: tar from $1 failed"
		return 1
	fi
	return 0
}

# --- oracle / measured run (lane D) -----------------------------------------

cmd_oracle() {
	profile=${1:?oracle needs a profile}
	out=${2:-${PERF_LEGDIR:-$PWD}}
	mkdir -p "$out"
	profile_params "$profile" >/dev/null 2>&1 || fail "unknown profile $profile"
	if [ "$DRY" = 1 ]; then
		say "oracle: profile=$profile (dry; in-VM fio-verify integrity fixture, ${ORACLE_MIB} MiB)"
		exit 0
	fi
	tree=$(prereq_checks)
	command -v sshpass >/dev/null 2>&1 || fail "sshpass missing (VM access)"
	ensure_vm "$tree" || fail "VM not reachable on $VM_HOST:$VM_PORT (set PERF_W1_VM_PORT / start it)"
	vm_prereqs || fail "VM prerequisites missing (ublk_drv / /dev/ublk-control / $FIO_BIN)"
	bin=$(build_bench "$tree") || fail "bench build failed"
	rbin=$(ship_bench "$bin") || fail "bench ship failed"
	device_lock_acquire "$out"
	vmdir="/tmp/w1-oracle-${PERF_RUN_ID:-manual}-$profile"
	vm_ssh "sudo -n rm -rf '$vmdir' && mkdir -p '$vmdir'" || fail "cannot prepare VM workdir $vmdir"
	say "oracle: running the integrity fixture in the VM ($vmdir)"
	oracle_rc=0
	vm_ssh "cd '$vmdir' && sudo -n env W1_ORACLE=1 W1_OUTDIR='$vmdir' W1_BACKEND='$vmdir/backend.img' W1_ORACLE_SIZE_MIB='$ORACLE_MIB' W1_FIO='$FIO_BIN' '$rbin' -test.run '^TestW1Oracle\$' -test.v -test.timeout ${TIMEOUT}s" >"$out/oracle.log" 2>&1 || oracle_rc=$?
	device_lock_release
	vm_ssh "sudo -n chmod -R a+rX '$vmdir'" >/dev/null 2>&1 || true
	collect_dir "$vmdir" "$out/vm" || true
	[ "$KEEP" = 1 ] || vm_ssh "sudo -n rm -rf '$vmdir'" >/dev/null 2>&1 || true
	if [ -f "$out/vm/meta.json" ]; then
		verdict=$(python3 - "$out/vm/meta.json" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
print("ok" if m.get("ok") else ("setup" if m.get("error_class") == "setup" else "bad"))
PY
		)
	else
		verdict=bad
	fi
	case "$verdict" in
	ok)    say "oracle: integrity fixture passed"; exit 0 ;;
	setup) say "oracle: setup failure (rc=$oracle_rc; log: $out/oracle.log)"; exit 40 ;;
	*)     say "oracle: integrity fixture failed (rc=$oracle_rc; log: $out/oracle.log)"; exit 30 ;;
	esac
}

cmd_run() {
	profile=${1:?run needs a profile}
	leg=${2:?run needs a leg}
	block=${3:?run needs a block}
	legdir=${4:?run needs a leg-dir}
	mkdir -p "$legdir"

	params=$(profile_params "$profile") || fail "unknown profile $profile"
	set -- $params
	rw=$1
	bs=$2
	shift 2
	fio_extra=""
	prefill=0
	for tok in ${1:+$*}; do
		case "$tok" in
		-) : ;;
		--prefill) prefill=1 ;;
		--*) fio_extra="$fio_extra ${tok#--}" ;;
		*) fio_extra="$fio_extra $tok" ;;
		esac
	done

	if [ "$DRY" = 1 ]; then
		say "run: profile=$profile leg=$leg block=$block class=$CLASS rw=$rw bs=$bs prefill=$prefill size=${SIZE_MIB}MiB runtime=${RUNTIME}s -> $legdir (dry)"
		exit 0
	fi

	tree=$(prereq_checks)
	command -v sshpass >/dev/null 2>&1 || fail "sshpass missing (VM access)"
	ensure_vm "$tree" || fail "VM not reachable on $VM_HOST:$VM_PORT (set PERF_W1_VM_PORT / start it)"
	vm_prereqs || fail "VM prerequisites missing (ublk_drv / /dev/ublk-control / $FIO_BIN)"
	bin=$(build_bench "$tree") || fail "bench build failed"
	rbin=$(ship_bench "$bin") || fail "bench ship failed"

	job_in="$legdir/fio.job.in"
	{
		cat <<EOF
[global]
ioengine=io_uring
direct=1
time_based=1
group_reporting=1
runtime=$RUNTIME
lat_percentiles=1
percentile_list=50:95:99:99.9:99.99
filename=%DEV%
[bench]
rw=$rw
bs=$bs
EOF
		for e in $fio_extra; do printf '%s\n' "$e"; done
	} >"$job_in" || fail "job template write failed"

	pre_job="$legdir/prefill.job.in"
	if [ "$prefill" = 1 ]; then
		{
			cat <<EOF
[global]
ioengine=io_uring
direct=1
group_reporting=1
filename=%DEV%
[prefill]
rw=write
bs=1m
size=${SIZE_MIB}m
EOF
		} >"$pre_job" || fail "prefill template write failed"
	fi

	vmdir="/tmp/w1-leg-${PERF_RUN_ID:-manual}-$block-$leg"
	vm_ssh "sudo -n rm -rf '$vmdir' && mkdir -p '$vmdir'" || fail "cannot prepare VM workdir $vmdir"
	vm_scp "$job_in" "$VM_HOST:$vmdir/fio.job.in" >/dev/null || fail "job template ship failed"
	if [ "$prefill" = 1 ]; then
		vm_scp "$pre_job" "$VM_HOST:$vmdir/prefill.job.in" >/dev/null || fail "prefill template ship failed"
	fi

	# Device window: held across the measured VM run only (lane C's shared lock).
	device_lock_acquire "$legdir"

	loadavg=$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null | tr -d '\n' || echo n/a)
	run_rc=0
	say "run: profile=$profile leg=$leg block=$block in the VM ($vmdir)"
	vm_ssh "cd '$vmdir' && sudo -n env W1_BENCH=1 W1_PROFILE='$profile' W1_OUTDIR='$vmdir' W1_BACKEND='$vmdir/backend.img' W1_SIZE_MIB='$SIZE_MIB' W1_JOB='$vmdir/fio.job.in' W1_PREFILL='$prefill' W1_PREFILL_JOB='$vmdir/prefill.job.in' W1_FIO='$FIO_BIN' '$rbin' -test.run '^TestW1Bench\$' -test.v -test.timeout ${TIMEOUT}s" >"$legdir/vm-run.log" 2>&1 || run_rc=$?
	device_lock_release
	vm_ssh "sudo -n chmod -R a+rX '$vmdir'" >/dev/null 2>&1 || true
	collect_dir "$vmdir" "$legdir/vm" || say "run: artifact collection failed (records kept)"
	[ "$KEEP" = 1 ] || vm_ssh "sudo -n rm -rf '$vmdir'" >/dev/null 2>&1 || true

	side=${PERF_SIDE:-}
	if [ -z "$side" ]; then
		case "$leg" in
		A*) side=baseline ;;
		B*) side=candidate ;;
		*)  side=candidate ;;
		esac
	fi
	stage=${PERF_STAGE:-measure}

	erc=0
	python3 "$EMIT" --vm-dir "$legdir/vm" --legdir "$legdir" --workload W1 \
		--profile "$profile" --side "$side" --leg "$leg" --block "$block" \
		--stage "$stage" --host-loadavg "$loadavg" --class "$CLASS" \
		>>"$legdir/samples.jsonl" || erc=$?
	if [ "$erc" != 0 ]; then
		say "run: leg failed (bench rc=$run_rc, emit rc=$erc; log: $legdir/vm-run.log)"
		exit "$erc"
	fi
	if [ "$run_rc" != 0 ]; then
		say "run: bench rc=$run_rc but meta is ok (records kept)"
		exit 30
	fi
	exit 0
}

cmd_profile() { # lane C region: matched diagnostics via the capture wrapper (never gating)
	profile=${1:?profile needs a profile}
	legdir=${2:-${PERF_LEGDIR:-$PWD}}
	mkdir -p "$legdir"
	if [ -z "$CAPTURE" ]; then
		say "profile: no capture wrapper — diagnostics skipped (never gating)"
		exit 0
	fi
	if [ "$DRY" = 1 ]; then
		say "profile: would capture via $CAPTURE run diagnostic w1-$profile $legdir (dry)"
		exit 0
	fi
	if [ -z "${PERF_W1_PROFILE_CMD:-}" ]; then
		say "profile: PERF_W1_PROFILE_CMD unset (bench body is lane D's) — diagnostics skipped (never gating)"
		exit 0
	fi
	# ublk kprobe substitute needs the module loaded (no ublk:* tracepoints here)
	modprobe ublk_drv 2>/dev/null || true
	# Profiling stays separate from measured runs (design §6); no device-window
	# hold for diagnostics unless the profiled command itself touches host devices.
	exec "$CAPTURE" run diagnostic "w1-$profile" "$legdir" -- sh -c "$PERF_W1_PROFILE_CMD"
}

mode=${1:-}
[ -n "$mode" ] || fail "usage: w1.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir> | build"
shift
case "$mode" in
list)
	# D's v0.3 grid: 8 base + modify + 4 mixes + trim + write-fsync + the qd axis.
	echo seqread-4k
	echo seqread-128k
	echo seqwrite-4k
	echo seqwrite-128k
	echo randread-4k
	echo randread-128k
	echo randwrite-4k
	echo randwrite-128k
	echo randwrite-modify-4k
	echo randrw-50-4k
	echo randrw-50-128k
	echo randrw-70-4k
	echo randrw-70-128k
	echo trim-4k
	echo write-fsync-4k
	echo randread-4k-qd1
	echo randread-4k-qd32
	;;
oracle)  cmd_oracle "$@" ;;
run)     cmd_run "$@" ;;
profile) cmd_profile "$@" ;;
build)   tree=$(prereq_checks); build_bench "$tree" || exit 40 ;;
*) fail "unknown subcommand: $mode" ;;
esac
