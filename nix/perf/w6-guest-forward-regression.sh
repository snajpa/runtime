#!/bin/sh
# w6-guest-forward-regression.sh — lane A; reviewer1 3242/3243/3254.
#
# Self-contained forwarding/provenance regression for the W6 guest-capture path.
# Builds a deterministic TEST-LOCAL fixture (no external probe dirs) and forwards it
# through the real `feed_leg` (extracted from perf-suite.sh) and the real
# schema/emit.sh:
#   cold 7 guest capabilities (perf.guest, fio.guest, bpftrace.guest, tracepoint.ublk,
#   flamegraph.guest, psi.guest, guest.env) -> cold phase sample -> warm 7 guest
#   capabilities -> warm phase sample -> stop sample = 17 records.
# Asserts: completion + offset advancement (samples.jsonl.off = 17), 17/17 reach the
# stream in order, all 14 guest capabilities carry scope:"guest" + the supplied
# side/leg + cold|warm capture_phase, and no guest `record:"env"`. Negative bite:
# the legacy non-contract env record must abort the forward (non-zero rc, no offset
# watermark) with the emitter's env diagnostic.
# Run for side/leg variants candidate/B2 and baseline/A2.
#
# Exit: 0 = all pass; 1 = regression failure; 2 = harness/setup error.
# Overrides: W6_REGR_SUITE, W6_REGR_EMITTER, W6_REGR_DIR.
set -u

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SUITE=${W6_REGR_SUITE:-$DIR/perf-suite.sh}
EMITTER=${W6_REGR_EMITTER:-$DIR/schema/emit.sh}
R=${W6_REGR_DIR:-$HOME/ai/logs/e2b-perf/w6-forward-regression}

command -v bash >/dev/null 2>&1 || { echo "harness: bash not found" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "harness: python3 not found" >&2; exit 2; }
[ -f "$SUITE" ] || { echo "harness: suite not found: $SUITE" >&2; exit 2; }
[ -f "$EMITTER" ] || { echo "harness: emitter not found: $EMITTER" >&2; exit 2; }

rm -rf "$R"; mkdir -p "$R"

# --- extract the real functions -------------------------------------------------
sed -n '/^emit() {/,/^}/p' "$SUITE" > "$R/emit.fn"
sed -n '/^feed_leg() {/,/^}/p' "$SUITE" > "$R/feed_leg.fn"
grep -q '^emit() {' "$R/emit.fn" || { echo "harness: emit() not found in $SUITE" >&2; exit 2; }
grep -q '^feed_leg() {' "$R/feed_leg.fn" || { echo "harness: feed_leg() not found in $SUITE" >&2; exit 2; }

cat > "$R/feed-driver.sh" <<'EOD'
#!/usr/bin/env bash
set -eu
emit_fn=$1; feed_fn=$2; emitter=$3; stream=$4; run_id=$5; legdir=$6
die() { echo "die: $*" >&2; exit 40; }
. "$emit_fn"
. "$feed_fn"
EMITTER=$emitter
STREAM=$stream
RUN_ID=$run_id
SUITE_SHA=selftest
feed_leg "$legdir" run clean
EOD
chmod 755 "$R/feed-driver.sh"
RUN_ID=run-20260919T000000

fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }

# --- deterministic test-local fixture ------------------------------------------
caps() { # $1 side $2 leg $3 phase -> 7 guest capability records
	s=$1; l=$2; p=$3
	cat <<EOF
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"perf.guest","requirement":"to_execute","status":"available","version":"perf version 7.0.14","kernel_frames":1}
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"fio.guest","requirement":"to_execute","status":"available","version":"fio-3.36"}
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"bpftrace.guest","requirement":"validity","status":"available","version":"bpftrace v0.20.2"}
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"tracepoint.ublk","requirement":"validity","status":"unavailable","substitute":{"for":"tracepoint.ublk","via":"bpftrace-kprobe","symbols":80}}
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"flamegraph.guest","requirement":"validity","status":"unavailable","substitute":{"for":"flamegraph.guest","via":"store-copy-injection"}}
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"psi.guest","requirement":"optional","status":"available"}
{"record":"capability","scope":"guest","side":"$s","leg":"$l","capture_phase":"$p","name":"guest.env","requirement":"optional","status":"available","kernel":"7.0.0-31-generic","mem_available_kb":26238972,"modules":"nbd,ublk_drv,","devices":"ublk-control,,kvm,,"}
EOF
}
sample() { # $1 side $2 leg $3 metric-id $4 unit-id $5 value
	printf '{"record":"sample","workload":"W6","profile":"cold-warm-stop","side":"%s","leg":"%s","block":1,"stage":"measure","metric":{"id":"%s","unit":"s"},"unit_id":"%s","chunk":{"i":1,"n":1},"samples":[%s]}\n' \
		"$1" "$2" "$3" "$4" "$5"
}
gen_chain() { # $1 dir $2 side $3 leg -> 17-record samples.jsonl
	d=$1; s=$2; l=$3
	mkdir -p "$d"
	{ caps "$s" "$l" cold; sample "$s" "$l" cold_bringup_s w6-cold 914; \
	  caps "$s" "$l" warm; sample "$s" "$l" warm_bringup_s w6-warm 8; \
	  sample "$s" "$l" stop_s w6-stop 7; } > "$d/samples.jsonl"
}
gen_bite() { # $1 dir $2 side $3 leg -> chain with the legacy non-contract env record
	d=$1; s=$2; l=$3
	mkdir -p "$d"
	{ caps "$s" "$l" cold
	  printf '%s\n' "{\"record\":\"env\",\"kernel\":\"7.0.0-31-generic\",\"mem_available_kb\":26238972,\"modules\":\"nbd,ublk_drv,\",\"devices\":\"ublk-control,,kvm,,\",\"host_loadavg\":\"1.0 1.0 1.0\",\"port\":2233}"
	  sample "$s" "$l" cold_bringup_s w6-cold 914; } > "$d/samples.jsonl"
}

assert_stream() { # $1 stream $2 side $3 leg
	python3 - "$1" "$2" "$3" <<'PY'
import json, sys
st, side, leg = sys.argv[1], sys.argv[2], sys.argv[3]
recs = [json.loads(l) for l in open(st, encoding="utf-8") if l.strip()]
errs = []
if len(recs) != 17:
    errs.append("count %d != 17" % len(recs))
names = ["perf.guest", "fio.guest", "bpftrace.guest", "tracepoint.ublk",
         "flamegraph.guest", "psi.guest", "guest.env"]
exp = []
for ph in ("cold", "warm"):
    for n in names:
        exp.append(("capability", n, ph))
    exp.append(("sample", {"cold": "cold_bringup_s", "warm": "warm_bringup_s"}[ph], None))
exp.append(("sample", "stop_s", None))
for i, (kind, nm, ph) in enumerate(exp):
    if i >= len(recs):
        break
    r = recs[i]
    if r.get("record") != kind:
        errs.append("rec %d record %r != %r" % (i + 1, r.get("record"), kind)); continue
    if kind == "capability":
        if r.get("name") != nm: errs.append("rec %d name %r != %r" % (i + 1, r.get("name"), nm))
        if r.get("scope") != "guest": errs.append("rec %d scope %r" % (i + 1, r.get("scope")))
        if r.get("side") != side: errs.append("rec %d side %r != %r" % (i + 1, r.get("side"), side))
        if r.get("leg") != leg: errs.append("rec %d leg %r != %r" % (i + 1, r.get("leg"), leg))
        if r.get("capture_phase") != ph: errs.append("rec %d capture_phase %r != %r" % (i + 1, r.get("capture_phase"), ph))
    else:
        if r.get("metric", {}).get("id") != nm: errs.append("rec %d metric %r != %r" % (i + 1, r.get("metric", {}).get("id"), nm))
        if r.get("side") != side or r.get("leg") != leg: errs.append("rec %d side/leg mismatch" % (i + 1))
        if r.get("stage") != "measure": errs.append("rec %d stage %r" % (i + 1, r.get("stage")))
if sum(1 for r in recs if r.get("record") == "capability") != 14:
    errs.append("guest capability count != 14")
if any(r.get("record") == "env" for r in recs):
    errs.append("guest env record present")
if errs:
    print("FAIL stream %s: %s" % (st, "; ".join(errs)))
    sys.exit(1)
print("ok   17/17 in order; 14 caps scope/side/leg/capture_phase ok; no guest env (%s)" % st)
PY
}

run_chain() { # $1 tag $2 side $3 leg
	tag=$1; s=$2; l=$3
	d="$R/chain-$tag"; st="$R/stream-$tag.jsonl"
	gen_chain "$d" "$s" "$l"
	if bash "$R/feed-driver.sh" "$R/emit.fn" "$R/feed_leg.fn" "$EMITTER" "$st" "$RUN_ID" "$d" >"$R/$tag.out" 2>"$R/$tag.err"; then
		ok "forward completed ($tag)"
	else
		rc=$?
		bad "forward FAILED rc=$rc ($tag)"; sed -n '1,5p' "$R/$tag.err"
		return 1
	fi
	off=$(cat "$d/samples.jsonl.off" 2>/dev/null || echo '')
	if [ "$off" = "17" ]; then ok "offset advanced to 17 ($tag)"; else bad "offset '$off' != 17 ($tag)"; fi
	if assert_stream "$st" "$s" "$l"; then ok "stream assertions ($tag)"; else bad "stream assertions ($tag)"; fi
	# re-feed (ABBA revisit semantics): nothing new may be forwarded
	before=$(wc -l < "$st" | tr -d ' ')
	if bash "$R/feed-driver.sh" "$R/emit.fn" "$R/feed_leg.fn" "$EMITTER" "$st" "$RUN_ID" "$d" >"$R/$tag.refeed.out" 2>"$R/$tag.refeed.err"; then
		ok "re-feed rc 0 ($tag)"
	else
		bad "re-feed rc != 0 ($tag)"
	fi
	after=$(wc -l < "$st" | tr -d ' ')
	if [ "$before" = "$after" ]; then ok "re-feed forwards nothing new ($tag)"; else bad "re-feed added records ($tag: $before -> $after)"; fi
}

run_bite() { # legacy env record must fail closed
	d="$R/bite"; st="$R/stream-bite.jsonl"
	gen_bite "$d" candidate B2
	rc=0
	bash "$R/feed-driver.sh" "$R/emit.fn" "$R/feed_leg.fn" "$EMITTER" "$st" "$RUN_ID" "$d" >"$R/bite.out" 2>"$R/bite.err" || rc=$?
	if [ "$rc" -ne 0 ]; then ok "legacy env bite: forward failed rc=$rc"; else bad "legacy env bite: forward unexpectedly succeeded"; fi
	if [ -f "$d/samples.jsonl.off" ]; then bad "legacy env bite: offset watermark written"; else ok "legacy env bite: no offset watermark"; fi
	if grep -q 'missing required field for env' "$R/bite.err"; then ok "legacy env bite: emitter env diagnostic present"; else bad "legacy env bite: emitter diagnostic missing"; sed -n '1,5p' "$R/bite.err"; fi
	n=$(wc -l < "$st" 2>/dev/null | tr -d ' ' || echo 0)
	if [ "${n:-0}" -lt 17 ]; then ok "legacy env bite: stream truncated at the bad record ($n lines)"; else bad "legacy env bite: stream has $n lines"; fi
}

echo "w6-guest-forward-regression: suite=$SUITE emitter=$EMITTER scratch=$R"
run_chain cand-B2 candidate B2
run_chain base-A2 baseline A2
run_bite
echo "---- fixture/stream hashes ----"
sha256sum "$R"/chain-*/samples.jsonl "$R"/stream-*.jsonl 2>/dev/null

if [ "$fails" -eq 0 ]; then
	echo "w6-guest-forward-regression: PASS"
	exit 0
fi
echo "w6-guest-forward-regression: FAIL ($fails)"
exit 1
