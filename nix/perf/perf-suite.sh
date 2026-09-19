#!/usr/bin/env bash
# perf-suite.sh — R23 storage performance-regression suite (orchestrator).
#
# Frozen design: R23 v1.0 (2026-09-19, reviewer1 v1-PASS). Fixed protocol —
# 2 warmups + 12 randomized ABBA/BAAB blocks per cell×class; fixed-band A/A
# resolution with the WARN-vs-FAIL discriminability gate; five outcomes with
# distinct exits:
#
#   PASS 0 · FAIL 10 · WARN 20 · INCONCLUSIVE 30 · SETUP_ERROR 40
#
# Subcommands:
#   calibrate  --candidate <dir|ref> --env quiet|busy [--workloads W1,W4] [--profiles p1,p2] [--blocks N] [--seed N]
#   run        --candidate <dir|ref> [--baseline <dir|ref>] --env quiet|busy
#              [--workloads ...] [--blocks 12] [--warmups 2] [--seed N]
#              [--dry-run]
#   compare    --run <run-id|run-dir> [--baseline <store>]
#   flamegraph --run <run-id> [--mode kernel|user|combined]
#   report     --run <run-id|run-dir> [--baseline <p>] (lane F)
#   report     --verify --run <run-id|run-dir> (lane F)
#   selftest
#
# Contracts (owners — see nix/perf/README.md):
#   schema/emit.sh    lane F — append canonical perf/2 records to T1.
#   schema/compare.sh lane F — verdicts from the stream vs the baseline store.
#   modules/<w>.sh    lanes  — W1 lane D · W4 lane B · W6 lane A (W3/W5 TBD).
#   trees.sh          lane A — materialize/validate the baseline+candidate trees.
#   device lock       lane C — same flock contract for device-touching workloads.
#
# Artifacts: ~/ai/logs/e2b-perf/<run-id>/ (never tmpfs; 40 GB floor).

set -eu

PERF_ROOT=$(cd -- "$(dirname -- "$0")" && pwd)
MODULES_DIR="${PERF_MODULES_DIR:-$PERF_ROOT/modules}"
EMITTER="${PERF_EMITTER:-$PERF_ROOT/schema/emit.sh}"
COMPARATOR="${PERF_COMPARATOR:-$PERF_ROOT/schema/compare.sh}"
TREES_HELPER="${PERF_TREES_HELPER:-$PERF_ROOT/trees.sh}"
LOG_ROOT="${PERF_LOG_ROOT:-$HOME/ai/logs/e2b-perf}"
SUITE_SHA=$(git -C "$PERF_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)

EXIT_PASS=0; EXIT_FAIL=10; EXIT_WARN=20; EXIT_INCONCLUSIVE=30; EXIT_SETUP_ERROR=40
DEFAULT_BLOCKS=12; DEFAULT_WARMUPS=2
FLOOR_KB=$((40 * 1024 * 1024))

say() { printf 'perf-suite: %s\n' "$*" >&2; }
die() { printf 'perf-suite: %s\n' "$*" >&2; exit "$EXIT_SETUP_ERROR"; }

disk_guard() {
	local dir="$1" fs avail_kb
	mkdir -p -- "$dir" || die "cannot create $dir"
	fs=$(findmnt -no FSTYPE --target "$dir" 2>/dev/null || echo unknown)
	[ "$fs" != tmpfs ] || die "artifact dir is RAM-backed (tmpfs): $dir"
	avail_kb=$(df -Pk -- "$dir" | awk 'NR==2 {print $4}')
	if [ -n "$avail_kb" ] && [ "$avail_kb" -lt "$FLOOR_KB" ]; then
		die "below the 40 GB floor at $dir"
	fi
}

new_run_id() { printf 'run-%s' "$(date -u +%Y%m%dT%H%M%S)"; }


suite_lock_acquire() { # suite-level heavy-run lock (lane E 2273/2280) — $LOG_ROOT/.suite.lock
	if [ -n "${SUITE_LOCK_HELD:-}" ]; then return 0; fi
	local lock_wait t0 waited=0
	lock_wait="${PERF_SUITE_WAIT:-0}"
	case "$lock_wait" in ''|*[!0-9]*) lock_wait=0 ;; esac
	mkdir -p -- "$LOG_ROOT" 2>/dev/null || true
	exec 9>"$LOG_ROOT/.suite.lock" || die "suite lock: cannot open $LOG_ROOT/.suite.lock"
	if [ "$lock_wait" -gt 0 ]; then
		t0=$(date -u +%s)
		flock -w "$lock_wait" 9 || die "another suite run holds $LOG_ROOT/.suite.lock (waited ${lock_wait}s)"
		waited=$(( $(date -u +%s) - t0 ))
	else
		flock -n 9 || die "another suite run holds $LOG_ROOT/.suite.lock (set PERF_SUITE_WAIT=<s> to wait)"
	fi
	SUITE_LOCK_HELD=1
	PERF_SUITE_LOCK_WAIT_S=$waited
	export PERF_SUITE_LOCK_WAIT_S
	if [ "$waited" -gt 0 ]; then say "suite lock acquired after ${waited}s wait"; fi
	return 0
}


list_workloads() {
	local w n
	for w in "$MODULES_DIR"/[Ww][0-9]*.sh; do
		[ -e "$w" ] || continue
		n=$(basename "$w" .sh)
		# W/w + all-decimal suffix only: helper scripts (e.g. w1-lock-regression.sh)
		# must not join default workload discovery (reviewer1 3297/3301).
		case "${n#?}" in ''|*[!0-9]*) continue ;; esac
		printf '%s\n' "$n" | tr 'a-z' 'A-Z'
	done
}

module_file() { # $1=workload token; resolves W4.sh / w4.sh
	local tok up lo
	up=$(printf '%s' "$1" | tr 'a-z' 'A-Z')
	lo=$(printf '%s' "$1" | tr 'A-Z' 'a-z')
	for tok in "$up" "$lo"; do
		if [ -f "$MODULES_DIR/$tok.sh" ]; then printf '%s' "$MODULES_DIR/$tok.sh"; return 0; fi
	done
	return 1
}

# Fixed block protocol: ABBA/BAAB chosen deterministically per block+seed.
block_legs() { # $1=block index $2=seed -> per-position leg tokens
	if [ $(( ($1 + $2) % 2 )) -eq 0 ]; then
		printf 'A1 B1 B2 A2'
	else
		printf 'B1 A1 A2 B2'
	fi
}

block_order() { # $1=block index $2=seed
	if [ $(( ($1 + $2) % 2 )) -eq 0 ]; then printf ABBA; else printf BAAB; fi
}

emit_oracle() { # $1=workload $2=profile $3=passed $4=exit(optional)
	# Cell gate for W7/W8: non-runtime namespace (reviewer1 2462) - eligibility/
	# setup evidence only; must never satisfy oracle_matches().
	local kind="$1/$2"
	case "$1" in W7|W8) kind="gate/$1/$2" ;; esac
	local body="{\"record\":\"oracle\",\"kind\":\"$kind\",\"passed\":$3"
	[ -n "${4:-}" ] && body="$body,\"exit\":$4"
	body="$body}"
	printf '%s' "$body" | emit oracle run clean
}

emit_block() { # $1=workload $2=profile $3=index $4=warmups $5=order $6=seed
	local warmup=false
	[ "$3" -le "$4" ] && warmup=true
	local body="{\"record\":\"block\",\"workload\":\"$1\",\"profile\":\"$2\",\"index\":$3,\"warmup\":$warmup,\"order\":\"$5\",\"namespace\":\"ns-$RUN_ID-b$3\",\"seed\":$6}"
	printf '%s' "$body" | emit block run clean
}

feed_leg() { # $1=leg-dir $2=phase $3=mode - feed NEW record-bearing lines to T1
	local f="$1/samples.jsonl" off_file="$1/samples.jsonl.off" line kind last off
	[ -f "$f" ] || return 0
	last=0
	[ -f "$off_file" ] && last=$(cat "$off_file" 2>/dev/null || echo 0)
	case "$last" in ''|*[!0-9]*) last=0 ;; esac
	off=$(wc -l < "$f" 2>/dev/null | tr -d " ")
	case "$off" in ''|*[!0-9]*) off=0 ;; esac
	[ "$off" -ge "$last" ] || last=0
	tail -n +$((last + 1)) "$f" | while IFS= read -r line; do
		[ -n "$line" ] || continue
		kind=$(printf "%s" "$line" | sed -n 's/.*"record"[[:space:]]*:[[:space:]]*"\([a-z_]*\)".*/\1/p')
		if [ -z "$kind" ]; then
			kind=note
			line="{\"record\":\"note\",\"text\":$(printf "%s" "$line" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}"
		fi
		printf "%s" "$line" | emit "$kind" "$2" "$3"
	done
	printf "%s" "$off" > "$off_file"
}

feed_setup() { # $1=setup-dir $2=phase $3=mode - forward setup records once (W7/W8; reviewer1 2463)
	local f="$1/samples.jsonl" off_file="$1.setup-off" line kind last off
	[ -f "$f" ] || return 0
	last=0
	[ -f "$off_file" ] && last=$(cat "$off_file" 2>/dev/null || echo 0)
	case "$last" in ''|*[!0-9]*) last=0 ;; esac
	off=$(wc -l < "$f" 2>/dev/null | tr -d " ")
	case "$off" in ''|*[!0-9]*) off=0 ;; esac
	[ "$off" -ge "$last" ] || last=0
	tail -n +$((last + 1)) "$f" | while IFS= read -r line; do
		[ -n "$line" ] || continue
		kind=$(printf "%s" "$line" | sed -n 's/.*"record"[[:space:]]*:[[:space:]]*"\([a-z_]*\)".*/\1/p')
		if [ -z "$kind" ]; then
			kind=note
			line="{\"record\":\"note\",\"text\":$(printf "%s" "$line" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}"
		fi
		printf "%s" "$line" | emit "$kind" "$2" "$3"
	done
	printf "%s" "$off" > "$off_file"
}

emit() { # $1=kind $2=phase $3=mode; body JSON on stdin -> T1 (lane F emit.sh)
	[ -x "$EMITTER" ] || die "emitter not landed yet (lane F): $EMITTER"
	PERF_SUITE_REF="perf-suite" PERF_SUITE_SHA="$SUITE_SHA" \
		"$EMITTER" append --stream "$STREAM" --kind "$1" --phase "$2" --mode "$3" --run-id "$RUN_ID"
}

# --- busy-class reference load (lane E) --------------------------------------
BUSY_STARTED=0; BUSY_STATE=""; BUSY_OUT=""
busy_start() { # $1 = run dir; starts the pinned reference load for --env busy
	# Per-host busy-class definition (reviewer1 2194): declared before the run;
	# a changed value is a new class → its own A/A, never tuned post-outcome.
	local rate=${PERF_BUSY_RATE:-2000} io=${PERF_BUSY_IO_CPUS:-0-1} cpu=${PERF_BUSY_CPU_CPUS:-8-15}
	local workers=${PERF_BUSY_CPU_WORKERS:-8} floor=${PERF_BUSY_LOAD_FLOOR:-0}
	local adapt=${PERF_BUSY_ADAPTIVE_MULT:-0} frac=${PERF_BUSY_ADAPTIVE_FLOOR_FRAC:-auto}
	BUSY_STATE="$1/busy-ref.state"; BUSY_OUT="$1/busy-load.json"
	"$PERF_ROOT/busy-ref.sh" start --rate "$rate" --io-cpus "$io" --cpu-cpus "$cpu" \
		--cpu-workers "$workers" --load-floor "$floor" \
		--adaptive-mult "$adapt" --adaptive-floor-frac "$frac" \
		--out "$BUSY_OUT" --state "$BUSY_STATE" >&2 || return 1
	BUSY_STARTED=1  # session exists before settle-check: a settle miss must still be stopped/recorded (E 2923)
	local srt=0
	"$PERF_ROOT/busy-ref.sh" settle-check --state "$BUSY_STATE" >&2 || srt=$?
	[ "$srt" = 0 ] || return "$srt"
}
busy_stop_emit() { # stop the load; emit the busy env record; 30 on envelope miss
	local rc=0 reason="" fp free_gb
	[ "$BUSY_STARTED" = 1 ] || return 0  # idempotent: no started session — nothing to stop/emit (E 2923)
	"$PERF_ROOT/busy-ref.sh" stop --state "$BUSY_STATE" >&2 || rc=$?
	if [ "$rc" != 0 ] && [ "$rc" != 3 ]; then
		# failed stop: the load state is unproven — keep BUSY_STARTED (an installed
		# EXIT trap may retry) and report the failure to the caller (E 2923)
		return "$rc"
	fi
	[ "$rc" = 3 ] && reason="busy envelope miss (load signature outside envelope)"
	fp=$(sh "$PERF_ROOT/perf-env-capture.sh" "$RUN_DIR" 2>/dev/null | sed -n 's/^fingerprint_sha256: //p')
	[ -n "${fp:-}" ] || fp=$( { cat /proc/cmdline /proc/cpuinfo; } 2>/dev/null | sha256sum | cut -d' ' -f1)
	free_gb=$(df -Pk -- "$RUN_DIR" | awk 'NR==2 {printf "%.1f", $4/1048576}')
	python3 - "$BUSY_OUT" "$fp" "$free_gb" "$reason" "$RUN_DIR" <<'PY' | emit env run clean || return "$?"
import json, sys
j = json.load(open(sys.argv[1]))
rec = {"record": "env", "env_class": "busy",
       "host": {"fingerprint": sys.argv[2]},
       "tools": {},
       "disk_guard": {"path": sys.argv[5], "free_gb": float(sys.argv[3]),
                      "floor_gb": 40, "tmpfs": False},
       "busy_load": {"recipe": j.get("recipe", "fio-fixed+stressng"),
                     "params_digest": j.get("params_digest", "")}}
if sys.argv[4]:
    rec["invalid_reason"] = sys.argv[4]
print(json.dumps(rec))
PY
	BUSY_STARTED=0
	[ "$rc" = 3 ] && return 30
	return 0
}

cmd_selftest() {
	say "selftest: syntax + block plan"
	bash -n "$0"
	local i seed=7
	for i in 1 2 3 4; do
		say "  block $i: $(block_legs "$i" "$seed")"
	done
	# feed_leg bite: ABBA revisits the same legdir; only NEW lines may forward
	local sd; sd=$(mktemp -d)
	mkdir -p "$sd/block1-A"
	printf '%s\n' '{"record":"note","text":"one"}' > "$sd/block1-A/samples.jsonl"
	local sv_stream="${STREAM:-}" sv_run="${RUN_ID:-}" sv_sha="${SUITE_SHA:-}"
	STREAM="$sd/perf-runs.jsonl" RUN_ID=run-20260919T000000 SUITE_SHA=selftest
	feed_leg "$sd/block1-A" run clean
	printf '%s\n' '{"record":"note","text":"two"}' >> "$sd/block1-A/samples.jsonl"
	feed_leg "$sd/block1-A" run clean
	feed_leg "$sd/block1-A" run clean
	local sn; sn=$(wc -l < "$sd/perf-runs.jsonl" 2>/dev/null || echo 0)
	STREAM="$sv_stream" RUN_ID="$sv_run" SUITE_SHA="$sv_sha"
	if [ "$sn" != "2" ]; then say "  feed_leg ABBA-revisit bite FAILED ($sn != 2)"; rm -rf "$sd"; return 1; fi
	say "  feed_leg forwards only the delta across revisits (2 records)"
	rm -rf "$sd"
	# source guard (lane C 2905): no persistent-redirect no-command exec
	"$PERF_ROOT/source-guard.sh" || { say "  source-guard FAILED"; return 1; }
	# w1 device-lock stderr regression (lane C 2922): fixed visible / buggy suppressed
	"$PERF_ROOT/modules/w1-lock-regression.sh" || { say "  w1 lock regression FAILED"; return 1; }
	# default discovery must skip non-workload helpers, keep W<n> workloads (A 3290; reviewer1 3297/3301)
	local wl; wl=$(list_workloads | paste -sd, -)
	case ",$wl," in
		*",W1-LOCK-REGRESSION,"*) say "  workload discovery includes non-workload helper W1-LOCK-REGRESSION FAILED"; return 1 ;;
	esac
	case ",$wl," in
		*",W1,"*) ;;
		*) say "  workload discovery lost real workload W1 FAILED"; return 1 ;;
	esac
	say "  workload discovery keeps W<n> real workloads and excludes non-workload helpers"
	say "selftest OK"
}

cmd_run() {
	local candidate="" baseline="" env_class="" workloads="" profiles_filter=""
	local blocks=$DEFAULT_BLOCKS warmups=$DEFAULT_WARMUPS
	local seed=$(( $(date -u +%s) % 100000 )) dry=0
	while [ $# -gt 0 ]; do
		case "$1" in
			--candidate) candidate=$2; shift 2 ;;
			--baseline)  baseline=$2;  shift 2 ;;
			--env)       env_class=$2; shift 2 ;;
			--workloads) workloads=$2; shift 2 ;;
			--blocks)    blocks=$2;    shift 2 ;;
			--warmups)   warmups=$2;   shift 2 ;;
			--seed)      seed=$2;      shift 2 ;;
			--profiles)  profiles_filter=$2; shift 2 ;;
			--dry-run)   dry=1;        shift ;;
			*) die "run: unknown argument '$1'" ;;
		esac
	done
	[ -n "$candidate" ] || die "run: --candidate <dir|ref> is required"
	[ "$env_class" = quiet ] || [ "$env_class" = busy ] || die "run: --env quiet|busy is required"

	[ "$dry" -eq 1 ] || suite_lock_acquire
	RUN_ID=$(new_run_id); RUN_DIR="$LOG_ROOT/$RUN_ID"; STREAM="$RUN_DIR/perf-runs.jsonl"
	[ "$dry" -eq 1 ] || disk_guard "$RUN_DIR"
	if [ "$dry" -eq 0 ] && [ "$env_class" = busy ]; then
		trap 'trap_rc=0; busy_stop_emit || trap_rc=$?; [ "$trap_rc" = 0 ] || [ "$trap_rc" = 30 ] || say "busy: EXIT cleanup failed (rc=$trap_rc) — the load may still be running"' EXIT
		brc=0; busy_start "$RUN_DIR" || brc=$?
		if [ "$brc" != 0 ]; then
			cleanup_rc=0; busy_stop_emit || cleanup_rc=$?
			if [ "$cleanup_rc" != 0 ] && [ "$cleanup_rc" != 30 ]; then
				say "busy: cleanup failed after failed start (rc=$cleanup_rc) — run outcome SETUP_ERROR (exit 40); the EXIT handler retries"
				exit "$EXIT_SETUP_ERROR"
			fi
			if [ "$BUSY_STARTED" = 0 ]; then trap - EXIT; fi
			if [ "$brc" != 3 ]; then
				say "busy: reference load start/settle failed (rc=$brc) — run outcome SETUP_ERROR (exit 40)"
				exit "$EXIT_SETUP_ERROR"
			fi
			say "busy: settle floor miss — run outcome INCONCLUSIVE (exit 30)"
			exit 30
		fi
	fi
	# base env record — quiet load covariates (E 2210; lane A perf-env-record.sh)
	[ "$dry" -eq 1 ] || sh "$PERF_ROOT/perf-env-record.sh" "$env_class" "$RUN_DIR" | emit env run clean
	# suite lock wait — recorded only when the bounded wait actually blocked (E 2273)
	if [ "$dry" -eq 0 ] && [ "${PERF_SUITE_LOCK_WAIT_S:-0}" -gt 0 ]; then
		printf '{"record":"note","text":"suite lock wait_s=%s"}' "$PERF_SUITE_LOCK_WAIT_S" | emit note run clean || true
	fi
	# host capability records — probe matrix (lane C; ruling 9)
	if [ "$dry" -eq 0 ] && [ -x "$PERF_ROOT/perf-capture.sh" ]; then
		"$PERF_ROOT/perf-capture.sh" capabilities | while IFS= read -r l; do
			[ -n "$l" ] || continue
			printf '%s\n' "$l" | emit capability run clean || exit 40
		done || die "host capability emit failed"
	fi

	[ -n "$workloads" ] || workloads=$(list_workloads | paste -sd, -)
	[ -n "$workloads" ] || die "run: no workload modules in $MODULES_DIR"

	say "run $RUN_ID: env=$env_class candidate=$candidate baseline=${baseline:-<envelope>}"
	say "blocks=$blocks warmups=$warmups seed=$seed workloads=$workloads"

	local w p b leg legs legdir order tdir rc mfile stage side
	for w in ${workloads//,/ }; do
		mfile=$(module_file "$w") || die "run: no module for workload $w"
		for p in $(bash "$mfile" list 9>&-); do
			if [ -n "$profiles_filter" ]; then
				case ",$profiles_filter," in *",$p,"*) ;; *) continue ;; esac
			fi
			if [ "$dry" -eq 1 ]; then
				say "  plan: $w/$p — oracle, then $((warmups+blocks)) blocks ($(block_order 1 "$seed")/…)"
				continue
			fi
			# step 0: correctness oracle per cell — failure skips the cell
			if PERF_ENV_CLASS="$env_class" PERF_TREE_DIR="$candidate" PERF_RUN_ID="$RUN_ID" PERF_PHASE=run \
			   PERF_MODE=clean PERF_SUITE_REF="perf-suite" PERF_SUITE_SHA="$SUITE_SHA" \
			   bash "$mfile" oracle "$p" "$RUN_DIR/$w-$p-oracle" 9>&- >>"$RUN_DIR/$w-$p-oracle.log" 2>&1; then
				emit_oracle "$w" "$p" true
				case "$w" in
					W7|W8) feed_setup "$RUN_DIR/$w-$p-oracle" run clean ;;
				esac
			else
				rc=$?
				emit_oracle "$w" "$p" false "$rc"
				say "  oracle failed for $w/$p (rc=$rc) — cell skipped"
				continue
			fi
			for (( b = 1; b <= blocks + warmups; b++ )); do
				order=$(block_order "$b" "$seed")
				stage=measure
				[ "$b" -le "$warmups" ] && stage=warmup
				emit_block "$w" "$p" "$b" "$warmups" "$order" "$seed"
				for leg in $(block_legs "$b" "$seed"); do
					legdir="$RUN_DIR/$w/$p/block$b-$leg"
					mkdir -p "$legdir"
					case "$leg" in
						A*) tdir=${baseline:-$candidate}; [ -n "$baseline" ] && side=baseline || side=candidate ;;
						B*) tdir=$candidate; side=candidate ;;
					esac
					if PERF_ENV_CLASS="$env_class" PERF_LEG="$leg" PERF_SIDE="$side" PERF_TREE_DIR="$tdir" \
					   PERF_STAGE="$stage" \
					   PERF_RUN_ID="$RUN_ID" PERF_PHASE=run PERF_MODE=clean \
					   PERF_SUITE_REF="perf-suite" PERF_SUITE_SHA="$SUITE_SHA" \
					   bash "$mfile" run "$p" "$leg" "$b" "$legdir" 9>&-; then
						feed_leg "$legdir" run clean
					else
						rc=$?
						say "  $w/$p block$b leg$leg rc=$rc (records kept)"
						feed_leg "$legdir" run clean
					fi
				done
			done
		done
	done
	[ "$dry" -eq 1 ] && { say "dry-run complete (no measurements taken)"; return 0; }
	busy_rc=0; busy_stop_emit || busy_rc=$?
	if [ "$busy_rc" = 30 ]; then
		if [ "$BUSY_STARTED" = 0 ]; then trap - EXIT; fi
		say "busy: envelope miss recorded — run outcome INCONCLUSIVE (exit 30)"
		exit 30
	fi
	if [ "$busy_rc" != 0 ]; then
		say "busy: load cleanup failed (rc=$busy_rc) — run outcome SETUP_ERROR (exit 40); the EXIT handler retries"
		exit "$EXIT_SETUP_ERROR"
	fi
	if [ "$BUSY_STARTED" = 0 ]; then trap - EXIT; fi
	say "run complete: $RUN_DIR"
}

cmd_calibrate() {
	local candidate="" env_class="" workloads="" profiles="" blocks="" warmups="" seed="" dry=0
	while [ $# -gt 0 ]; do
		case "$1" in
			--candidate) candidate=$2; shift 2 ;;
			--env)       env_class=$2; shift 2 ;;
			--workloads) workloads=$2; shift 2 ;;
			--profiles)  profiles=$2; shift 2 ;;
			--blocks)    blocks=$2; shift 2 ;;
			--warmups)   warmups=$2; shift 2 ;;
			--seed)      seed=$2; shift 2 ;;
			--dry-run)   dry=1; shift ;;
			*) die "calibrate: unknown argument '$1'" ;;
		esac
	done
	[ -n "$candidate" ] || die "calibrate: --candidate <dir|ref> is required (the A/A pass runs both sides on it)"
	[ "$env_class" = quiet ] || [ "$env_class" = busy ] || die "calibrate: --env quiet|busy is required"
	[ -x "$COMPARATOR" ] || die "comparator not landed yet (lane F): $COMPARATOR"

	# A/A pass: both sides = the same tree; reuses the run loop verbatim so
	# oracle-first, the modules, the busy_start/busy_stop_emit hooks and the
	# emitter behave exactly as in `run` (envelope miss exits 30 there).
	set -- --candidate "$candidate" --baseline "$candidate" --env "$env_class"
	[ -n "$workloads" ] && set -- "$@" --workloads "$workloads"
	[ -n "$profiles" ] && set -- "$@" --profiles "$profiles"
	[ -n "$blocks" ] && set -- "$@" --blocks "$blocks"
	[ -n "$warmups" ] && set -- "$@" --warmups "$warmups"
	[ -n "$seed" ] && set -- "$@" --seed "$seed"
	if [ "$dry" -eq 1 ]; then
		cmd_run "$@" --dry-run
		say "calibration dry-run complete (no stream; comparator skipped)"
		return 0
	fi
	cmd_run "$@"
	say "calibration A/A pass complete: $RUN_DIR"
	"$COMPARATOR" calibrate --run-dir "$RUN_DIR" --emit --out "$RUN_DIR/resolution.json"
}

cmd_compare() {
	[ -x "$COMPARATOR" ] || die "comparator not landed yet (lane F): $COMPARATOR"
	local args=""
	while [ $# -gt 0 ]; do
		case "$1" in
			--run) args="$args --run-dir $LOG_ROOT/$2"; shift 2 ;;
			*)     args="$args $1"; shift ;;
		esac
	done
	# shellcheck disable=SC2086
	exec "$COMPARATOR" compare $args
}

cmd_flamegraph() {
	local run_id="" mode=combined
	while [ $# -gt 0 ]; do
		case "$1" in
			--run)  run_id=$2; shift 2 ;;
			--mode) mode=$2; shift 2 ;;
			*) die "flamegraph: unknown argument '$1'" ;;
		esac
	done
	[ -n "$run_id" ] || die "flamegraph: --run <run-id> is required"
	case "$mode" in combined|kernel|user) ;; *) die "flamegraph: --mode must be combined|kernel|user" ;; esac
	RUN_ID="$run_id"; RUN_DIR="$LOG_ROOT/$RUN_ID"; STREAM="$RUN_DIR/perf-runs.jsonl"
	[ -d "$RUN_DIR" ] || die "flamegraph: no such run: $RUN_DIR"
	local f d unit n files
	files=$(find "$RUN_DIR" -type f -name '*.perf.data' 2>/dev/null | sort)
	[ -n "$files" ] || die "flamegraph: no retained perf.data under $RUN_DIR"
	n=0
	for f in $files; do
		d=$(dirname -- "$f"); unit=$(basename -- "$f" .perf.data)
		"$PERF_ROOT/perf-capture.sh" flamegraph "$unit" "$d" "$mode" | while IFS= read -r line; do
			[ -n "$line" ] || continue
			printf '%s\n' "$line" | emit artifact diagnose diagnostic
		done
		n=$((n + 1))
	done
	say "flamegraph: regenerated $n diagnostic unit(s) under $RUN_DIR"
}
cmd_report() {
	[ -x "$COMPARATOR" ] || die "comparator not landed yet (lane F): $COMPARATOR"
	local action=replay dir="" extra="" verify=0
	while [ $# -gt 0 ]; do
		case "$1" in
			--run)    dir=$2; shift 2 ;;
			--verify) verify=1; shift ;;
			*)        extra="$extra $1"; shift ;;
		esac
	done
	[ -n "$dir" ] || die "report: --run <run-id|run-dir> is required"
	[ -d "$dir" ] || dir="$LOG_ROOT/$dir"
	[ "$verify" -eq 1 ] && action=verify
	exec "$COMPARATOR" "$action" --run-dir "$dir" $extra
}

usage() { sed -n '2,26p' "$0"; }

case "${1:-}" in
	calibrate)  shift; cmd_calibrate "$@" ;;
	run)        shift; cmd_run "$@" ;;
	compare)    shift; cmd_compare "$@" ;;
	flamegraph) shift; cmd_flamegraph "$@" ;;
	report)     shift; cmd_report "$@" ;;
	selftest)   cmd_selftest ;;
	""|-h|--help) usage ;;
	*) die "unknown subcommand '$1' (try --help)" ;;
esac
