#!/bin/sh
# perf-capture selftest — bite checks for lane C's capture wrapper: capability
# probe, device-lock contention + wait recording, clean/trace/diagnostic units,
# flamegraph regeneration, and acceptance of every emitted body by lane F's
# perf/2 emitter. Exit 0 = all pass; artifacts under ~/ai/logs/e2b-perf (never
# tmpfs). Override the scratch root with PERF_CAPTURE_SELFTEST_DIR.
set -u
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
R=${PERF_CAPTURE_SELFTEST_DIR:-$HOME/ai/logs/e2b-perf/capture-selftest-$$}
EMIT="$DIR/schema/emit.sh"
rm -rf "$R"
mkdir -p "$R"
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }

export PERF_RUN_ID=run-20260919T000000
export PERF_SUITE_REF=perf-suite
export PERF_SUITE_SHA=selftest

feed() { # read perf/2 bodies on stdin; emit each through lane F's emitter
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		k=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["record"])' 2>/dev/null)
		if [ -z "$k" ]; then
			bad "unparseable record: $line"
			continue
		fi
		if printf '%s\n' "$line" | "$EMIT" append --stream "$R/perf-runs.jsonl" --kind "$k" --phase run --mode clean >/dev/null 2>&1; then
			ok "emit accepted: $k"
		else
			bad "emit rejected: $k :: $line"
		fi
	done
}

# 1) capability probe
"$DIR/perf-capture.sh" capabilities | feed

# 2) device lock under contention (hold 2 s, run a locked unit)
LOCK=${E2B_NBD_TEST_LOCK:-${TMPDIR:-/tmp}/e2b-nbd-device-tests.lock}
flock -x "$LOCK" -c 'sleep 2' &
holder=$!
sleep 0.2
"$DIR/perf-capture.sh" run clean locked "$R/lock" --lock -- sleep 0.1 >"$R/locked.out"
wait "$holder"
w=$(sed -n 's/.*"wait_s":\([0-9.]*\).*/\1/p' "$R/locked.out" | head -1)
if grep -q '"acquired":true' "$R/locked.out" && awk -v w="$w" 'BEGIN{exit !(w >= 1.5)}'; then
	ok "lock contention wait recorded: ${w}s"
else
	bad "lock note/contention wait wrong (wait_s=${w:-none})"
fi
sed -n '1p' "$R/locked.out" | feed
sed -n '2p' "$R/locked.out" | feed

# 3) trace unit (block IO) + unit logs
IOF="$R/io.bin" "$DIR/perf-capture.sh" run trace tr "$R" -- sh -c 'dd if=/dev/zero of="$IOF" bs=1M count=16 status=none; sync; cat "$IOF" >/dev/null' | feed
[ -f "$R/logs/tr.stdout" ] && ok "trace unit logs retained" || bad "trace unit logs missing"

# 4) diagnostic unit + flamegraph regeneration
"$DIR/perf-capture.sh" run diagnostic diag "$R" -- sh -c 'i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done' | feed
[ -s "$R/diag.svg" ] && ok "diagnostic SVG generated" || bad "diagnostic SVG missing"
"$DIR/perf-capture.sh" flamegraph diag "$R" | feed
"$DIR/perf-capture.sh" flamegraph diag "$R" kernel | feed
[ -s "$R/diag.kernel.svg" ] && ok "kernel-mode SVG generated" || bad "kernel-mode SVG missing"

echo "capture-selftest: fails=$fails (artifacts: $R)"
exit "$fails"
