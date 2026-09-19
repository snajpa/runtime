#!/bin/sh
# W4 module - migration benches (perf-regression suite; lane B).
# Contract (nix/perf/README.md):
#   list                         -> profile ids (one per line)
#   oracle <profile> [outdir]    -> step-0 correctness gate (migrate counters + reconcile)
#   run <profile> <leg> <block> <leg-dir>
#                                -> one measured block; raw record appended to
#                                   <leg-dir>/samples.jsonl; artifacts in <leg-dir>; no verdicts
#   profile <profile> <leg-dir>  -> matched-diagnostics capture (never gating)
# Env: PERF_ENV_CLASS, PERF_TREE_DIR / PERF_TREES_DIR, PERF_LEG, PERF_SIDE, PERF_STAGE,
#      PERF_RUN_ID/PERF_PHASE/PERF_MODE/PERF_SUITE_*, PERF_W4_FIXTURES (fixture cache),
#      PERF_W4_TOOL (tool override), PERF_W4_ORACLE_CLASS, PERF_W4_DRY, PERF_CAPTURE.
# Exit: 0 ok / 30 inconclusive / 40 setup_error (suite scheme).
set -u

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
PY="$HERE/w4"
FIXTURES=${PERF_W4_FIXTURES:-"$HOME/ai/logs/e2b-perf/fixtures"}
TREES=${PERF_TREES_DIR:-/root/ai/worktrees/e2b}
DRY=${PERF_W4_DRY:-0}
ENV_CLASS=${PERF_ENV_CLASS:-quiet}

say() { echo "+ $*" >&2; }

# profile -> "<store class> <compress|-> <concurrency> <tool mode>"
profile_params() {
	case "$1" in
	header-c1)       echo "large - 1 migrate" ;;
	zstd-c1)         echo "large zstd 1 migrate" ;;
	zstd-c4)         echo "large zstd 4 migrate" ;;
	latency-zstd-c1) echo "latency zstd 1 migrate" ;;
	reconcile-c1)    echo "latency - 1 reconcile" ;;
	*) return 1 ;;
	esac
}

kinds_of() {
	case "$1" in
	tiny|latency) echo 2 ;;
	large)        echo 1 ;;
	*) return 1 ;;
	esac
}

ensure_store() {
	class=$1
	if [ -f "$FIXTURES/$class.ids" ]; then
		return 0
	fi

	say "fixture: generating $class at $FIXTURES/$class"
	if ! sh "$HERE/../fixtures/genstore.sh" --class "$class" \
		--out "$FIXTURES/$class" --ids "$FIXTURES/$class.ids"; then
		echo "w4: fixture generation failed for $class" >&2
		return 1
	fi
	[ -f "$FIXTURES/$class.ids" ] || {
		echo "w4: fixture generation incomplete for $class (no ids file)" >&2
		return 1
	}
}

resolve_tool() {
	leg=$1
	# per-position leg tokens (A1/B2/…) share the side's tree + tool cache
	case "$leg" in
	A*) side=A ;;
	B*) side=B ;;
	*)  side=$leg ;;
	esac
	if [ -n "${PERF_W4_TOOL:-}" ]; then
		echo "$PERF_W4_TOOL"
		return 0
	fi

	tree=${PERF_TREE_DIR:-"$TREES/perf-$side"}
	if [ ! -d "$tree" ]; then
		echo "w4: no tree dir $tree (set PERF_TREE_DIR or materialize it via trees.sh)" >&2
		return 1
	fi

	bin="$FIXTURES/.bin/migrate-builds-$side"
	log="$FIXTURES/.bin/build-$side.log"
	mkdir -p "$FIXTURES/.bin"

	if [ ! -x "$bin" ]; then
		say "tool: building migrate-builds from $tree"
		tmp="$bin.tmp.$$"
		if ! (cd "$tree" && go build -o "$tmp" ./packages/orchestrator/cmd/migrate-builds) \
			>"$log" 2>&1; then
			echo "w4: tool build failed for side $side (leg $leg; log: $log)" >&2
			if [ -s "$log" ]; then
				tail -n 20 "$log" >&2
			else
				echo "w4: (build produced no output)" >&2
			fi
			rm -f "$tmp"
			return 1
		fi
		mv -f "$tmp" "$bin"
	fi

	echo "$bin"
}

cmd_list() {
	echo header-c1
	echo zstd-c1
	echo zstd-c4
	echo latency-zstd-c1
	echo reconcile-c1
}

cmd_oracle() {
	profile=${1:?oracle needs a profile}
	out=${2:-${PERF_LEGDIR:-$PWD}}
	class=${PERF_W4_ORACLE_CLASS:-tiny}
	mkdir -p "$out"

	if [ "$DRY" = 1 ]; then
		say "oracle: profile=$profile class=$class store=$FIXTURES/$class out=$out"
		exit 0
	fi

	ensure_store "$class" || exit 40

	tool=$(resolve_tool "${PERF_LEG:-candidate}") || exit 40

	exec python3 "$PY/w4-oracle.py" --tool "$tool" --store "$FIXTURES/$class" \
		--ids "$FIXTURES/$class.ids" --kinds "$(kinds_of "$class")" --out "$out"
}

cmd_run() {
	profile=${1:?run needs a profile}
	leg=${2:?run needs a leg}
	block=${3:?run needs a block}
	legdir=${4:?run needs a leg-dir}

	set -- $(profile_params "$profile") || { echo "w4: unknown profile $profile" >&2; exit 40; }
	class=$1
	compress=$2
	conc=$3
	tmode=$4
	[ "$compress" = "-" ] && compress=""
	mkdir -p "$legdir"

	if [ "$DRY" = 1 ]; then
		say "run: profile=$profile leg=$leg block=$block class=$class compress=${compress:-none} conc=$conc mode=$tmode -> $legdir"
		exit 0
	fi

	ensure_store "$class" || exit 40

	tool=$(resolve_tool "$leg") || exit 40

	python3 "$PY/w4-block.py" --tool "$tool" --store "$FIXTURES/$class" \
		--ids "$FIXTURES/$class.ids" --kinds "$(kinds_of "$class")" --out "$legdir" \
		--profile "$profile" --leg "$leg" --block "$block" --compress "$compress" \
		--concurrency "$conc" --tool-mode "$tmode" --store-class "$class" \
		--class "$ENV_CLASS"
}

cmd_profile() {
	profile=${1:?profile needs a profile}
	legdir=${2:?profile needs a leg-dir}

	cap=${PERF_CAPTURE:-"$HERE/../perf-capture.sh"}
	if [ ! -f "$cap" ]; then
		say "profile: no capture wrapper ($cap); diagnostics skipped (never gating)"
		exit 0
	fi

	set -- $(profile_params "$profile") || { echo "w4: unknown profile $profile" >&2; exit 40; }
	class=$1
	compress=$2
	conc=$3
	tmode=$4
	[ "$compress" = "-" ] && compress=""
	mkdir -p "$legdir"

	ensure_store "$class" || exit 40

	tool=$(resolve_tool "${PERF_LEG:-candidate}") || exit 40

	exec sh "$cap" run diagnostic w4 "$legdir" -- \
		python3 "$PY/w4-block.py" --tool "$tool" --store "$FIXTURES/$class" \
		--ids "$FIXTURES/$class.ids" --kinds "$(kinds_of "$class")" --out "$legdir" \
		--profile "$profile" --leg "${PERF_LEG:-candidate}" --block diag \
		--compress "$compress" --concurrency "$conc" --tool-mode "$tmode" \
		--store-class "$class" --class "$ENV_CLASS"
}

mode=${1:?usage: w4.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>}
shift

case "$mode" in
list)    cmd_list ;;
oracle)  cmd_oracle "$@" ;;
run)     cmd_run "$@" ;;
profile) cmd_profile "$@" ;;
*)       echo "usage: w4.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>" >&2; exit 2 ;;
esac
