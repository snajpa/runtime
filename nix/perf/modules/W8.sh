#!/bin/sh
# W8 module — post-resume run performance (perf-regression suite; lane A).
# RE-LAND DRAFT (NOT landed — wave freeze; land post-wave). Base: reverted scaffold
# `bb35854cb` (saved as W8.sh.reverted-20260919), folded to the v1.1-PASSED set:
#   runtime perf-suite `f4272351a` · design mirror `549d66bf5` · README section 11 `6c5bf9b46`.
# Evidence contract: ~/ai/logs/e2b-lane-f/w7w8-evidence-contract.md
# VM-side sequence: ~/ai/logs/e2b-lane-e/w7-w8-vm-side-spec-draft.md
# Contract (nix/perf/README.md):
#   list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>
#   list     -> profile ids (one per line): read | write | modify | mixed (op_class)
#   oracle   -> step-0: fixture identity read -> snapshot record (11.1 set) + `fixture/w8` oracle
#   run      -> one measured post-resume workload leg; records to <leg-dir>/samples.jsonl; no verdicts
#   profile  -> matched-diagnostics capture (never gating); post-confirmation only
#
# Measured sequence (oracle-first; lane E seam):
#   ready barrier (W7's StartTypeResume/envd ready; startup EXCLUDED from all timing)
#   -> pre-window identity oracle: marker / read-write identity check -> emit role=pre_identity
#   -> timed windows: first_io (first_io_latency_us + first_io_faults) and steady
#      (post_resume_lat_{p50,p95,p99}_us / post_resume_throughput_mib_s / post_resume_iops)
#      as separately named metrics
#   -> immediately post-workload integrity validation -> emit role=post_integrity
#   -> only then: samples, each with snapshot_id + class + profile_digest + workload_digest +
#      window=first_io|steady + op_class=read|write|modify|mixed
#   -> guest capture + capability scope=guest outside every timed span
# Runtime oracles (both required per measured restore; comparator stream-level):
#   {"record":"oracle","kind":"W8/pre-identity","workload":"W8","side":S,"snapshot_id":ID,
#    "class":CLS,"role":"pre_identity","profile_digest":PD,"workload_digest":WD,"passed":true}
#   {"record":"oracle","kind":"W8/post-integrity", ..., "role":"post_integrity", ...}
# Placement: the oracle outdir gets the `snapshot` record + `fixture/w8` oracle + `fixture`
#   artifact; the run legdir gets the runtime oracles + samples (feed_leg -> T1). Oracle-outdir
#   setup records go through the dedicated setup/snapshot forwarder (reviewer1 2461–2465;
#   dispatch side, lands post-wave). The snapshot record is emitted per run; a stream carrying
#   both W7 and W8 runs holds identical records for the same fixture (comparator keys by
#   snapshot_id — harmless).
# Non-device workload: no device-window lock. Role records are emitted by each measured `run` leg
#   for that leg's own side (reviewer1 2461–2465: no candidate-only path); the dispatch gate is
#   setup evidence only and never satisfies a runtime triple (2470).
# Env: PERF_ENV_CLASS, PERF_LEG, PERF_SIDE, PERF_STAGE, PERF_TREE_DIR / PERF_TREES_DIR,
#   PERF_RUN_ID/PERF_PHASE/PERF_MODE/PERF_SUITE_*, PERF_CAPTURE, PERF_W8_DRY,
#   PERF_W8_SNAPSHOT_DIR (defaults to PERF_W7_SNAPSHOT_DIR),
#   PERF_W8_READY_CMD / PERF_W8_WORKLOAD_CMD (lane E seams; provisional),
#   PERF_W8_PROFILE_DIGEST / PERF_W8_WORKLOAD_DIGEST (predeclared declarations; lane A/E).
# Exit: 0 ok / 30 inconclusive / 40 setup_error (suite scheme).
set -u

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
TREES=${PERF_TREES_DIR:-/root/ai/worktrees/e2b}
DRY=${PERF_W8_DRY:-0}
CAPTURE=${PERF_CAPTURE:-$HERE/../perf-capture.sh}
[ -x "$CAPTURE" ] || CAPTURE=""
FIXTURE_DIR=${PERF_W8_SNAPSHOT_DIR:-${PERF_W7_SNAPSHOT_DIR:-}}

say() { echo "+ $*" >&2; }
fail() { echo "w8: $*" >&2; exit 40; }

# profile -> op class (workload params/digests are predeclared per profile).
profile_params() {
  case "$1" in
  read|write|modify|mixed) echo "$1" ;;
  *) return 1 ;;
  esac
}

side_of_leg() { # position token -> sealed side (A1/A2 -> baseline, B1/B2 -> candidate)
  case "$1" in
  A*|a*) echo baseline ;;
  B*|b*) echo candidate ;;
  *) echo "" ;;
  esac
}

resolve_tree() {
  side=${PERF_SIDE:-}
  [ -n "$side" ] || side=$(side_of_leg "${PERF_LEG:-}")
  [ -n "$side" ] || side=candidate
  tree=${PERF_TREE_DIR:-"$TREES/perf-$side"}
  [ -d "$tree" ] || fail "no tree dir $tree (set PERF_TREE_DIR or materialize it)"
  echo "$tree"
}

fixture_json() { # printed path; written by the lane A fixture recorder (snapshot.json)
  [ -n "$FIXTURE_DIR" ] || fail "PERF_W8_SNAPSHOT_DIR (or PERF_W7_SNAPSHOT_DIR) unset — no snapshot fixture declared (section 11.1; lane A recorder deliverable)"
  f="$FIXTURE_DIR/snapshot.json"
  [ -f "$f" ] || fail "fixture identity missing: $f (lane A recorder: perf-w7w8-fixture.sh)"
  echo "$f"
}

prereq_checks() {
  tree=$(resolve_tree) || exit 40
  [ -d "$tree/packages/orchestrator/cmd/resume-build" ] || \
    fail "tree lacks packages/orchestrator/cmd/resume-build ($tree)"
  [ -n "${PERF_W8_READY_CMD:-}" ] || \
    fail "PERF_W8_READY_CMD unset — ready-barrier seam not declared (lane E)"
  [ -n "${PERF_W8_WORKLOAD_CMD:-}" ] || \
    fail "PERF_W8_WORKLOAD_CMD unset — in-VM workload seam not declared (lane E)"
  fixture_json >/dev/null
  echo "$tree"
}

emit_fixture_records() { # $1=oracle outdir $2=snapshot.json -> snapshot + fixture oracle
  out=$1; fj=$2
  python3 - "$fj" "$out" <<'PY' || return 40
import json, sys
fj, out = sys.argv[1], sys.argv[2]
req = ["snapshot_id", "content_sha256", "source_tree", "firecracker", "kernel", "rootfs",
       "init", "config", "vcpu", "memory_mib", "cpu_template", "network", "ports", "uffd",
       "object_store_state", "restore_state_dir", "quiesced_marker", "class",
       "cache_state_evidence", "reset_procedure"]
rec = json.load(open(fj))
missing = [k for k in req if k not in rec]
if missing:
    print("w8: snapshot.json lacks: %s" % ", ".join(missing), file=sys.stderr)
    sys.exit(40)
if not rec.get("snapshot_id") or not rec.get("content_sha256"):
    print("w8: snapshot.json needs a non-empty snapshot_id/content_sha256", file=sys.stderr)
    sys.exit(40)
snap = {"record": "snapshot"}
for k in req:
    snap[k] = rec[k]
orc = {"record": "oracle", "kind": "fixture/w8", "passed": True}
with open(out + "/samples.jsonl", "a") as f:
    f.write(json.dumps(snap, separators=(",", ":"), sort_keys=True) + "\n")
    f.write(json.dumps(orc, separators=(",", ":"), sort_keys=True) + "\n")
PY
}

cmd_oracle() {
  profile=${1:?oracle needs a profile}
  out=${2:-${PERF_LEGDIR:-$PWD}}
  mkdir -p "$out"
  op=$(profile_params "$profile") || fail "unknown profile $profile"
  if [ "$DRY" = 1 ]; then
    say "oracle: profile=$profile op_class=$op (dry): fixture identity read -> snapshot record + fixture oracle"
    exit 0
  fi
  prereq_checks >/dev/null
  fj=$(fixture_json)
  emit_fixture_records "$out" "$fj" || fail "fixture record emission failed (11.1 snapshot record)"
  cp -- "$fj" "$out/fixture.json" || fail "fixture copy failed"
  fsha=$(sha256sum "$out/fixture.json" | awk '{print $1}')
  printf '{"record":"artifact","kind":"fixture","path":"%s","sha256":"%s"}\n' "$out/fixture.json" "$fsha" >>"$out/samples.jsonl"
  say "oracle: fixture identity read for $profile; snapshot record + fixture oracle + fixture artifact -> $out"
  # NOTE: the per-restore pre/post oracles run in `run`, around the timed windows.
}

cmd_run() {
  profile=${1:?run needs a profile}
  leg=${2:?run needs a leg}
  block=${3:?run needs a block}
  legdir=${4:?run needs a leg-dir}
  mkdir -p "$legdir"
  op=$(profile_params "$profile") || fail "unknown profile $profile"
  stage=${PERF_STAGE:-measure}
  side=${PERF_SIDE:-}
  [ -n "$side" ] || side=$(side_of_leg "$leg")
  [ -n "$side" ] || fail "cannot resolve side for leg $leg"
  if [ "$DRY" = 1 ]; then
    say "run: profile=$profile op_class=$op leg=$leg block=$block stage=$stage side=$side -> $legdir (dry)"
    say "  plan: ready barrier (startup excluded) -> pre_identity oracle -> windows (first_io + steady) -> post_integrity oracle"
    say "  plan: samples only after both oracles; workload_digest/window/op_class linkage; no verdicts"
    exit 0
  fi
  prereq_checks >/dev/null
  # TODO(lane E seams PERF_W8_READY_CMD / PERF_W8_WORKLOAD_CMD + lane A): measured flow —
  #   1. assert/reach the W7 ready barrier; never time startup;
  #   2. pre-window identity oracle: marker / read-write identity check -> emit
  #      {"record":"oracle","kind":"W8/pre-identity","workload":"W8","side":$side,"snapshot_id":<snapshot.json>,"class":<snapshot.json class>,"role":"pre_identity","profile_digest":<decl>,"workload_digest":<decl>,"passed":true};
  #   3. timed windows via the lane E workload tooling: first_io + steady (separately named metrics);
  #   4. immediately post-workload integrity validation -> emit role=post_integrity;
  #   5. only then emit samples (window/op_class + linkage) to "$legdir/samples.jsonl"
  #      (stage=$stage); guest capability records (scope=guest, side=$side) outside every timed
  #      span; no verdicts.
  #   6. persist section 11.5 replay inputs: "$legdir/replay.json" {argv, env subset,
  #      block/leg/order, snapshot_id, class, profile_digest, workload_digest, window/op_class,
  #      replay_cmd} + an `artifact` record {kind:"replay",path,sha256} appended to
  #      "$legdir/samples.jsonl".
  #   Any oracle failure or missing seam => fail closed (40); no sample without both oracles.
  fail "run: post-resume workload seam not wired yet (reland draft; lane E seam)"
}

cmd_profile() { # diagnostics via the capture wrapper; never gating
  profile=${1?profile:?profile needs a profile}
  legdir=${2:-${PERF_LEGDIR:-$PWD}}
  mkdir -p "$legdir"
  if [ -z "$CAPTURE" ]; then
    say "profile: no capture wrapper — diagnostics skipped (never gating)"
    exit 0
  fi
  if [ "$DRY" = 1 ]; then
    say "profile: would capture via $CAPTURE run diagnostic w8-$profile $legdir (dry)"
    exit 0
  fi
  if [ -z "${PERF_W8_PROFILE_CMD:-}" ]; then
    say "profile: PERF_W8_PROFILE_CMD unset (workload seam is lane E's) — diagnostics skipped (never gating)"
    exit 0
  fi
  exec "$CAPTURE" run diagnostic "w8-$profile" "$legdir" -- sh -c "$PERF_W8_PROFILE_CMD"
}

mode=${1:-}
[ -n "$mode" ] || fail "usage: W8.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>"
shift
case "$mode" in
list)
  echo read
  echo write
  echo modify
  echo mixed
  ;;
oracle)  cmd_oracle "$@" ;;
run)     cmd_run "$@" ;;
profile) cmd_profile "$@" ;;
*) fail "unknown subcommand: $mode" ;;
esac
