#!/bin/sh
# perf/2 comparator selftest — bite cases: PASS/FAIL/WARN/INCONCLUSIVE/SETUP/verify.
set -u
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1 (rc=$2 want $3)"; fails=$((fails+1)); }

mkstream() { # $1=out $2=factor-low $3=factor-high (alternating when high given)
  python3 - "$1" "$2" "${3:-}" <<'PY'
import json,sys
out,lo,hi=sys.argv[1],float(sys.argv[2]),sys.argv[3]
hi=float(hi) if hi else None
with open(out,"w") as f:
    for b in range(1,13):
        fac = lo if (hi is None or b%2) else hi
        a=[100.0,102.0,98.0,100.0]
        bvals=[x*fac for x in a]
        f.write(json.dumps({"record":"sample","run_id":"run-20260919T000000","workload":"W1","profile":"p","side":"baseline","leg":"A","block":b,"stage":"measure","metric":{"id":"throughput_mib_s","unit":"MiB/s"},"unit_id":"u","chunk":{"i":1,"n":1},"samples":a})+"\n")
        f.write(json.dumps({"record":"sample","run_id":"run-20260919T000000","workload":"W1","profile":"p","side":"candidate","leg":"B","block":b,"stage":"measure","metric":{"id":"throughput_mib_s","unit":"MiB/s"},"unit_id":"u","chunk":{"i":1,"n":1},"samples":bvals})+"\n")
PY
}

expect() { # expect <rc> <name> <stream>
  rc=$1; name=$2; stream=$3
  sh "$DIR/compare.sh" compare --stream "$stream" --iters 400 --seed 7 >/dev/null 2>&1
  got=$?; [ "$got" = "$rc" ] && ok "$name" || bad "$name" "$got" "$rc"
}

mkstream "$TMP/pass.jsonl" 1.0
mkstream "$TMP/fail.jsonl" 0.94
mkstream "$TMP/warn.jsonl" 0.985
mkstream "$TMP/inc.jsonl" 0.96 1.04

expect 0  'PASS (A==B)'        "$TMP/pass.jsonl"
expect 10 'FAIL (-6%)'         "$TMP/fail.jsonl"
expect 20 'WARN (-1.5%)'       "$TMP/warn.jsonl"
expect 30 'INCONCLUSIVE (+-4%)' "$TMP/inc.jsonl"

sh "$DIR/compare.sh" compare --stream "$TMP/nope.jsonl" --iters 200 >/dev/null 2>&1
[ $? = 40 ] && ok 'SETUP_ERROR (missing stream)' || bad 'SETUP_ERROR (missing stream)' "$?" 40

mkdir -p "$TMP/run"
cp "$TMP/pass.jsonl" "$TMP/run/perf-runs.jsonl"
sh "$DIR/compare.sh" compare --run-dir "$TMP/run" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'compare --run-dir writes summary' || bad 'compare --run-dir writes summary' "$?" 0
sh "$DIR/compare.sh" verify --run-dir "$TMP/run" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'verify OK' || bad 'verify OK' "$?" 0
before=$(sha256sum "$TMP/run/perf-summary.json" | cut -d' ' -f1)
sh "$DIR/compare.sh" replay --run-dir "$TMP/run" --iters 400 --seed 7 >/dev/null 2>&1
after=$(sha256sum "$TMP/run/perf-summary.json" | cut -d' ' -f1)
[ "$before" = "$after" ] && ok 'replay is read-only' || bad 'replay is read-only' changed unchanged
python3 - "$TMP/run/perf-summary.json" <<'PY'
import json,sys
p=sys.argv[1]; d=json.load(open(p)); d["verdict"]="fail"; d["exit"]=10
json.dump(d, open(p,"w"), indent=1, sort_keys=True)
PY
sh "$DIR/compare.sh" verify --run-dir "$TMP/run" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 40 ] && ok 'verify DRIFT (tampered)' || bad 'verify DRIFT (tampered)' "$?" 40

# calibrate: tight null -> valid + emits resolution records; noisy null -> invalid
sh "$DIR/compare.sh" calibrate --stream "$TMP/pass.jsonl" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'calibrate valid (tight null)' || bad 'calibrate valid (tight null)' "$?" 0
before=$(wc -l < "$TMP/pass.jsonl")
sh "$DIR/compare.sh" calibrate --stream "$TMP/pass.jsonl" --iters 400 --seed 7 --emit >/dev/null 2>&1
after=$(wc -l < "$TMP/pass.jsonl")
[ $((after-before)) = 1 ] && ok 'calibrate --emit appends one resolution record' || bad 'calibrate --emit appends one resolution record' "$((after-before))" 1
sh "$DIR/compare.sh" calibrate --stream "$TMP/inc.jsonl" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'calibrate invalid (noisy null)' || bad 'calibrate invalid (noisy null)' "$?" 30

# warmup exclusion bites: skewed warmups must not contaminate statistics
python3 - "$TMP/warm.jsonl" <<'PY2'
import json, sys
out = sys.argv[1]
open(out, "w").close()
for b in range(1, 15):
    a = [100.0, 102.0, 98.0, 100.0]
    if b <= 2:
        fac, stage = (0.94 if b == 1 else 1.06), "warmup"
    else:
        fac, stage = 1.0, "measure"
    v = [x * fac for x in a]
    for leg in ("A", "B"):
        rec = {"record": "sample", "run_id": "run-20260919T000000", "workload": "W1",
               "profile": "p", "side": "baseline" if leg == "A" else "candidate",
               "leg": leg, "block": b, "stage": stage,
               "metric": {"id": "throughput_mib_s", "unit": "MiB/s"},
               "unit_id": "u", "chunk": {"i": 1, "n": 1},
               "samples": a if leg == "A" else v}
        open(out, "a").write(json.dumps(rec) + "\n")
PY2
sh "$DIR/compare.sh" calibrate --stream "$TMP/warm.jsonl" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'warmups excluded (calibrate valid)' || bad 'warmups excluded (calibrate valid)' "$?" 0
sed 's/"stage": "warmup"/"stage": "measure"/g' "$TMP/warm.jsonl" > "$TMP/warm2.jsonl"
sh "$DIR/compare.sh" calibrate --stream "$TMP/warm2.jsonl" --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'warmup inclusion would bite (control invalid)' || bad 'warmup inclusion would bite (control invalid)' "$?" 30

# duplicate identities must block any regression outcome (reviewer1 2175)
python3 - "$TMP/dup.jsonl" <<'PY3'
import json, sys
out = sys.argv[1]
open(out, "w").close()
base = {"record": "sample", "run_id": "run-20260919T000000", "workload": "W1",
        "profile": "p", "side": "baseline", "leg": "A", "block": 1,
        "stage": "measure", "metric": {"id": "throughput_mib_s", "unit": "MiB/s"},
        "unit_id": "u", "chunk": {"i": 1, "n": 1}, "samples": [100.0, 102.0]}
with open(out, "a") as f:
    for b in range(1, 13):
        rec = dict(base); rec["block"] = b
        f.write(json.dumps(rec) + "\n")
        f.write(json.dumps(rec) + "\n")
PY3
sh "$DIR/compare.sh" compare --stream "$TMP/dup.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'duplicate identities -> INCONCLUSIVE (rc 30)' || bad 'duplicate identities -> INCONCLUSIVE (rc 30)' "$?" 30
grep -q '"duplicate_identities": 12' "$TMP/perf-summary.json" && ok 'summary carries duplicate_identities' || bad 'summary carries duplicate_identities' missing present

# position legs (A1/A2/B1/B2): the comparator normalizes A*->A / B*->B when pairing
python3 - "$TMP/pos.jsonl" <<'PY3'
import json, sys
out = sys.argv[1]
base = {"record": "sample", "run_id": "run-20260919T000000", "workload": "W1",
        "profile": "p", "side": "baseline", "stage": "measure",
        "metric": {"id": "throughput_mib_s", "unit": "MiB/s"},
        "unit_id": "u", "chunk": {"i": 1, "n": 1}, "samples": [100.0, 102.0]}
with open(out, "w") as f:
    for b in range(1, 13):
        for leg in ("A1", "A2"):
            rec = dict(base); rec["block"] = b; rec["leg"] = leg
            f.write(json.dumps(rec) + "\n")
        for leg in ("B1", "B2"):
            rec = dict(base); rec["block"] = b; rec["leg"] = leg
            rec["side"] = "candidate"
            f.write(json.dumps(rec) + "\n")
PY3
sh "$DIR/compare.sh" compare --stream "$TMP/pos.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'position legs pair (A1/A2 vs B1/B2) -> PASS' || bad 'position legs pair' "$?" 0

# section 11 runtime-oracle linkage (reviewer1 2369/2370/2408/2409): per used
# (workload, side, snapshot, class) triple a role-matched, kind-family-matched,
# digest-matched oracle; W7 requires role=ready (post-ready marker/RW), W8
# requires pre_identity AND post_integrity.
python3 - "$TMP" <<'PY3'
import json, sys, os
T = sys.argv[1]
def snap(sid, fcver="1.12.1", tree="5eb54defe"):
    return {"record": "snapshot", "snapshot_id": sid, "content_sha256": "deadbeef",
            "source_tree": {"ref": tree}, "firecracker": {"version": fcver},
            "kernel": {"image": "vmlinux"}, "rootfs": {"path": "/r"},
            "init": {"binary": "/init"}, "config": {"cmdline": "console=ttyS0"},
            "vcpu": 2, "memory_mib": 1024, "cpu_template": "none",
            "network": {"mode": "tap"}, "ports": [80], "uffd": {"mode": "lazy"},
            "object_store_state": {"local": True}, "restore_state_dir": "/state",
            "quiesced_marker": {"sha256": "abc1234"},
            "class": "warm", "cache_state_evidence": {"page_cache": "warm"},
            "reset_procedure": {"drop_caches": True}}
def caps(guest_sides=("baseline", "candidate"), host_status="available", host_req="validity"):
    out = [{"record": "capability", "name": "host.perf", "status": host_status,
            "requirement": host_req, "scope": "host"}]
    for s in guest_sides:
        out.append({"record": "capability", "name": "guest.fio", "status": "available",
                    "requirement": "validity", "scope": "guest", "side": s})
    return out
def orc(wl, side, kind=None, role="ready", sid=None, cls="warm", pd="abc1234",
        wd=None, passed=True):
    o = {"record": "oracle", "kind": kind or (wl + "/restore"), "passed": passed,
         "workload": wl, "side": side,
         "snapshot_id": sid if sid is not None else ("snap-A" if side == "baseline" else "snap-B"),
         "class": cls, "role": role, "profile_digest": pd}
    if wd is not None:
        o["workload_digest"] = wd
    return o
def w7samples(bad_side=False):
    rs = []
    for b in range(1, 13):
        for leg in ("A1", "A2", "B1", "B2"):
            side = "baseline" if leg[0] == "A" else "candidate"
            r = {"record": "sample", "run_id": "run-20260919T000000", "workload": "W7",
                 "profile": "restore-warm", "side": side, "leg": leg, "block": b,
                 "stage": "measure", "metric": {"id": "restore_to_ready", "unit": "s"},
                 "unit_id": "restore", "chunk": {"i": 1, "n": 1}, "samples": [0.42],
                 "snapshot_id": "snap-A" if side == "baseline" else "snap-B",
                 "class": "warm", "profile_digest": "abc1234"}
            if bad_side and leg == "B1" and b == 1:
                r["side"] = "baseline"
            rs.append(r)
    return rs
def w8samples():
    rs = []
    for b in range(1, 13):
        for leg in ("A1", "A2", "B1", "B2"):
            side = "baseline" if leg[0] == "A" else "candidate"
            rs.append({"record": "sample", "run_id": "run-20260919T000000", "workload": "W8",
                       "profile": "post-resume-read", "side": side, "leg": leg, "block": b,
                       "stage": "measure",
                       "metric": {"id": "post_resume_lat_p95_us", "unit": "us"},
                       "unit_id": "window", "chunk": {"i": 1, "n": 1}, "samples": [123.0],
                       "snapshot_id": "snap-A" if side == "baseline" else "snap-B",
                       "class": "warm", "profile_digest": "abc1234",
                       "workload_digest": "beef1234", "window": "steady", "op_class": "read"})
    return rs
def env():
    return {"record": "env", "env_class": "quiet", "host": {"fingerprint": "abc1234"},
            "tools": {}, "disk_guard": {"path": "/x", "free_gb": 1, "floor_gb": 0, "tmpfs": False}}
def write(name, snaps, smp, orc_list, cap):
    with open(os.path.join(T, name), "w") as f:
        def w(o): f.write(json.dumps(o) + "\n")
        w(env())
        for s in snaps: w(s)
        for o in orc_list: w(o)
        for c in cap: w(c)
        for r in smp: w(r)
w7orcs = lambda: [orc("W7", "baseline", role="ready"), orc("W7", "candidate", role="ready")]
write("w7.jsonl", [snap("snap-A"), snap("snap-B", tree="bbbbbbb")], w7samples(), w7orcs(), caps())
write("w7-norole.jsonl", [snap("snap-A"), snap("snap-B")], w7samples(),
      [orc("W7", "baseline", role="pre_identity"), orc("W7", "candidate", role="pre_identity")], caps())
write("w7-unrelated.jsonl", [snap("snap-A"), snap("snap-B")], w7samples(),
      [orc("W4", "baseline", kind="W4/header-c1"), orc("W4", "candidate", kind="W4/header-c1")], caps())
write("w8.jsonl", [snap("snap-A"), snap("snap-B", tree="bbbbbbb")], w8samples(),
      [orc("W8", "baseline", kind="W8/post-resume", role="pre_identity", wd="beef1234"),
       orc("W8", "baseline", kind="W8/post-resume", role="post_integrity", wd="beef1234"),
       orc("W8", "candidate", kind="W8/post-resume", role="pre_identity", wd="beef1234"),
       orc("W8", "candidate", kind="W8/post-resume", role="post_integrity", wd="beef1234")], caps())
write("w8-ready.jsonl", [snap("snap-A"), snap("snap-B")], w8samples(),
      [orc("W8", "baseline", kind="W8/post-resume", role="pre_identity", wd="beef1234"),
       orc("W8", "candidate", kind="W8/post-resume", role="pre_identity", wd="beef1234")], caps())
write("w7-unsided.jsonl", [snap("snap-A"), snap("snap-B")], w7samples(),
      [{"record": "oracle", "kind": "W7/restore", "passed": True}], caps())
write("w7-aside.jsonl", [snap("snap-A"), snap("snap-B")], w7samples(),
      [orc("W7", "baseline", role="ready")], caps())
write("w7-badside.jsonl", [snap("snap-A"), snap("snap-B")], w7samples(bad_side=True), w7orcs(), caps())
write("w7-xm.jsonl", [snap("snap-A"), snap("snap-B", fcver="1.13.0")], w7samples(), w7orcs(), caps())
write("w7-noev.jsonl", [], w7samples(), w7orcs(), caps())
write("w7-toexec.jsonl", [snap("snap-A"), snap("snap-B")], w7samples(), w7orcs(),
      caps(host_status="unavailable", host_req="to_execute"))
PY3
sh "$DIR/compare.sh" compare --stream "$TMP/w7.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'W7 role-linked two-side evidence (trees differ) -> PASS' || bad 'W7 role-linked evidence' "$?" 0
sh "$DIR/compare.sh" compare --stream "$TMP/w7-norole.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 missing post-ready marker/RW role -> INCONCLUSIVE' || bad 'W7 missing ready role' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-unrelated.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 linked unrelated oracle kind -> INCONCLUSIVE' || bad 'W7 unrelated kind' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w8.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'W8 pre+post role-linked evidence -> PASS' || bad 'W8 pre+post evidence' "$?" 0
sh "$DIR/compare.sh" compare --stream "$TMP/w8-ready.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W8 ready-only / no post-workload integrity -> INCONCLUSIVE' || bad 'W8 ready-only' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-unsided.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 unsided generic oracle -> INCONCLUSIVE' || bad 'W7 unsided oracle' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-aside.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 only-baseline oracle -> INCONCLUSIVE' || bad 'W7 only-baseline oracle' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-badside.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 side-vs-leg mismatch -> INCONCLUSIVE' || bad 'W7 side-vs-leg mismatch' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-xm.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 cross-side config mismatch -> INCONCLUSIVE' || bad 'W7 cross-side mismatch' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-noev.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 30 ] && ok 'W7 without snapshot evidence -> INCONCLUSIVE' || bad 'W7 without snapshot evidence' "$?" 30
sh "$DIR/compare.sh" compare --stream "$TMP/w7-toexec.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 40 ] && ok 'W7 to_execute capability missing -> SETUP_ERROR' || bad 'W7 to_execute missing' "$?" 40

# unit_id grouping (reviewer1 2309): read/write series must not pool
python3 - "$TMP/uid.jsonl" <<'PY3'
import json, sys
out = sys.argv[1]
base = {"record": "sample", "run_id": "run-20260919T000000", "workload": "W1",
        "profile": "randrw-50-4k", "stage": "measure",
        "metric": {"id": "lat_p50_us", "unit": "us"}, "chunk": {"i": 1, "n": 1}}
with open(out, "w") as f:
    def w(o): f.write(json.dumps(o) + "\n")
    for b in range(1, 13):
        for leg, uid, v in (("A1", "read", 100.0), ("A2", "read", 100.0),
                            ("B1", "read", 100.0), ("B2", "read", 100.0),
                            ("A1", "write", 100.0), ("A2", "write", 100.0),
                            ("B1", "write", 150.0), ("B2", "write", 150.0)):
            r = dict(base); r["leg"] = leg; r["unit_id"] = uid
            r["side"] = "baseline" if leg[0] == "A" else "candidate"
            r["block"] = b; r["samples"] = [v]
            w(r)
PY3
sh "$DIR/compare.sh" compare --stream "$TMP/uid.jsonl" --iters 200 --seed 7 >/dev/null 2>&1
[ $? = 20 ] && ok 'unit_id series stay independent (read PASS + write WARN -> rc 20)' || bad 'unit_id series' "$?" 20
python3 - "$TMP/perf-summary.json" <<'PY3'
import json, sys
d = json.load(open(sys.argv[1]))
ks = [k for k in d.get("metrics", {}) if "lat_p50_us" in k]
sys.exit(0 if len(ks) == 2 else 1)
PY3
[ $? = 0 ] && ok 'unit_id bite: two independent rows' || bad 'unit_id bite' "$?" 0

# compare --run alias resolves a run id under PERF_LOG_ROOT
PERF_LOG_ROOT="$TMP/rootlogs" mkdir -p "$TMP/rootlogs/run-20260919T000000"
cp "$TMP/pass.jsonl" "$TMP/rootlogs/run-20260919T000000/perf-runs.jsonl"
PERF_LOG_ROOT="$TMP/rootlogs" sh "$DIR/compare.sh" compare --run run-20260919T000000 --iters 400 --seed 7 >/dev/null 2>&1
[ $? = 0 ] && ok 'compare --run <run-id> alias' || bad 'compare --run <run-id> alias' "$?" 0

rm -rf "$TMP"
if [ "$fails" = 0 ]; then echo 'selftest ok'; exit 0; fi
echo "selftest FAIL ($fails)"; exit 1
