#!/bin/sh
# W7 module — snapshot -> resume -> ready startup latency (perf-regression suite; lane A).
# RE-LAND DRAFT (NOT landed — wave freeze; land post-wave). Base: reverted scaffold
# `bb35854cb` (saved as W7.sh.reverted-20260919), folded to the v1.1-PASSED set:
#   runtime perf-suite `f4272351a` · design mirror `549d66bf5` · README section 11 `6c5bf9b46`.
# Evidence contract: ~/ai/logs/e2b-lane-f/w7w8-evidence-contract.md
# VM-side sequence: ~/ai/logs/e2b-lane-e/w7-w8-vm-side-spec-draft.md
# Contract (nix/perf/README.md):
#   list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>
#   list     -> profile ids (one per line): cold-uffd | warm-file (classes cold|warm)
#   oracle   -> step-0 fixture gate: fixture identity/quiesce read -> snapshot record (11.1 set)
#               + a generic `fixture/w7` oracle into the oracle outdir
#   run      -> one measured restore leg; perf/2 records to <leg-dir>/samples.jsonl; no verdicts
#   profile  -> matched-diagnostics capture (never gating); post-confirmation only
#
# Measured sequence (per restore; lane E seam):
#   t0 at the predeclared restore-invocation boundary
#   -> invoke restore (cold-uffd = UFFD-lazy resume / warm-file = file-backed warm resume)
#   -> subspans: restore_invocation_to_fc_restored (ms), fc_restored_to_ready (ms),
#      uffd_faults (count) + uffd_fetch_bytes (bytes) [persisted; never alter the primary]
#   -> ready = authoritative StartTypeResume/envd ready-to-serve (first SSH/ping diagnostic only)
#   -> post-ready marker / read-write identity check (must pass BEFORE the sample is emitted)
#   -> runtime oracle (role=ready) + sample `restore_to_ready` (s) + subspans
#   -> guest capture (lane E) + capability scope=guest, outside every timed span
# Runtime oracle (required per measured restore; comparator stream-level):
#   {"record":"oracle","kind":"W7/ready","workload":"W7","side":S,"snapshot_id":ID,
#    "class":CLS,"role":"ready","profile_digest":PD,"passed":true}
# Placement: the oracle outdir gets the `snapshot` record + `fixture/w7` oracle + `fixture`
#   artifact; the run legdir gets the runtime oracle + samples (feed_leg -> T1). Oracle-outdir
#   setup records go through the dedicated setup/snapshot forwarder (reviewer1 2461–2465;
#   dispatch side, lands post-wave).
# Non-device workload: no device-window lock. Role records are emitted by each measured `run` leg
#   for that leg's own side (reviewer1 2461–2465: no candidate-only path); the dispatch gate is
#   setup evidence only and never satisfies a runtime triple (2470).
# Env: PERF_ENV_CLASS, PERF_LEG, PERF_SIDE, PERF_STAGE, PERF_TREE_DIR / PERF_TREES_DIR,
#   PERF_RUN_ID/PERF_PHASE/PERF_MODE/PERF_SUITE_*, PERF_CAPTURE, PERF_W7_DRY,
#   PERF_W7_SNAPSHOT_DIR (fixture dir),
#   PERF_W7_RESTORE_CMD (restore invocation seam; lane E; provisional).
# Exit: 0 ok / 30 inconclusive / 40 setup_error (suite scheme).
set -u

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
TREES=${PERF_TREES_DIR:-/root/ai/worktrees/e2b}
DRY=${PERF_W7_DRY:-0}
CAPTURE=${PERF_CAPTURE:-$HERE/../perf-capture.sh}
[ -x "$CAPTURE" ] || CAPTURE=""
FIXTURE_DIR=${PERF_W7_SNAPSHOT_DIR:-}

say() { echo "+ $*" >&2; }
fail() { echo "w7: $*" >&2; exit 40; }

# profile -> "<class> <backend>" — immutable; the class feeds the snapshot record + samples.
profile_params() {
  case "$1" in
  cold-uffd) echo "cold uffd" ;;
  warm-file) echo "warm file" ;;
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
  [ -n "$FIXTURE_DIR" ] || fail "PERF_W7_SNAPSHOT_DIR unset — no snapshot fixture declared (section 11.1; lane A recorder deliverable)"
  f="$FIXTURE_DIR/snapshot.json"
  [ -f "$f" ] || fail "fixture identity missing: $f (lane A recorder: perf-w7w8-fixture.sh)"
  echo "$f"
}

prereq_checks() {
  tree=$(resolve_tree) || exit 40
  [ -d "$tree/packages/orchestrator/cmd/resume-build" ] || \
    fail "tree lacks packages/orchestrator/cmd/resume-build ($tree)"
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
    print("w7: snapshot.json lacks: %s" % ", ".join(missing), file=sys.stderr)
    sys.exit(40)
if not rec.get("snapshot_id") or not rec.get("content_sha256"):
    print("w7: snapshot.json needs a non-empty snapshot_id/content_sha256", file=sys.stderr)
    sys.exit(40)
snap = {"record": "snapshot"}
for k in req:
    snap[k] = rec[k]
orc = {"record": "oracle", "kind": "fixture/w7", "passed": True}
with open(out + "/samples.jsonl", "a") as f:
    f.write(json.dumps(snap, separators=(",", ":"), sort_keys=True) + "\n")
    f.write(json.dumps(orc, separators=(",", ":"), sort_keys=True) + "\n")
PY
}

cmd_oracle() {
  profile=${1:?oracle needs a profile}
  out=${2:-${PERF_LEGDIR:-$PWD}}
  mkdir -p "$out"
  pp=$(profile_params "$profile") || fail "unknown profile $profile"
  cls=${pp%% *}
  if [ "$DRY" = 1 ]; then
    say "oracle: profile=$profile class=$cls (dry): fixture identity/quiesce read -> snapshot record + fixture oracle"
    exit 0
  fi
  prereq_checks >/dev/null
  fj=$(fixture_json)
  emit_fixture_records "$out" "$fj" || fail "fixture record emission failed (11.1 snapshot record)"
  cp -- "$fj" "$out/fixture.json" || fail "fixture copy failed"
  fsha=$(sha256sum "$out/fixture.json" | awk '{print $1}')
  printf '{"record":"artifact","kind":"fixture","path":"%s","sha256":"%s"}\n' "$out/fixture.json" "$fsha" >>"$out/samples.jsonl"
  say "oracle: fixture identity read for $profile; snapshot record + fixture oracle + fixture artifact -> $out"
  # NOTE: the per-restore ready/marker proof is NOT here — it runs in `run`, on the measured
  # restore (every restored side proves its own ready/marker/RW integrity before a result).
}

cmd_run() {
  profile=${1:?run needs a profile}
  leg=${2:?run needs a leg}
  block=${3:?run needs a block}
  legdir=${4:?run needs a leg-dir}
  mkdir -p "$legdir"
  pp=$(profile_params "$profile") || fail "unknown profile $profile"
  cls=${pp%% *}
  backend=${pp#* }
  stage=${PERF_STAGE:-measure}
  side=${PERF_SIDE:-}
  [ -n "$side" ] || side=$(side_of_leg "$leg")
  [ -n "$side" ] || fail "cannot resolve side for leg $leg"
  if [ "$DRY" = 1 ]; then
    say "run: profile=$profile class=$cls leg=$leg block=$block stage=$stage side=$side -> $legdir (dry)"
    say "  plan: t0 boundary -> restore ($backend) -> subspans -> ready (StartTypeResume/envd)"
    say "  plan: post-ready marker/RW check -> oracle role=ready, THEN sample restore_to_ready (s) + subspans"
    say "  plan: guest capture + capability scope=guest outside the timed span; no verdicts"
    exit 0
  fi
  prereq_checks >/dev/null
  # TODO(lane E seam PERF_W7_RESTORE_CMD + lane A): measured flow —
  #   1. t0 at the predeclared invocation boundary; invoke the restore for this profile through
  #      the lane E hook (cold-uffd = UFFD-lazy resume / warm-file = file-backed warm resume);
  #   2. persist subspans (invocation -> FC restored, UFFD fault/bytes, FC -> ready) + the
  #      authoritative ready signal — never altering the primary timer;
  #   3. post-ready marker / read-write identity check; on pass emit the runtime oracle
  #      {"record":"oracle","kind":"W7/ready","workload":"W7","side":$side,"snapshot_id":<snapshot.json>,"class":$cls,"role":"ready","profile_digest":<profile declaration>,"passed":true};
  #      on failure: fail closed (40), never a sample;
  #   4. emit sample `restore_to_ready` (s) + subspan samples to "$legdir/samples.jsonl"
  #      (stage=$stage); guest capability records (scope=guest, side=$side) post-ready,
  #      outside every timed span; no verdicts.
  #   5. persist section 11.5 replay inputs: "$legdir/replay.json" {argv, env subset,
  #      block/leg/order, snapshot_id, class, profile_digest, replay_cmd} + an `artifact`
  #      record {kind:"replay",path,sha256} appended to "$legdir/samples.jsonl".
  fail "run: restore invocation/ready seam not wired yet (reland draft; lane E seam)"
}

cmd_profile() { # diagnostics via the capture wrapper; never gating
  profile=${1:?profile needs a profile}
  legdir=${2:-${PERF_LEGDIR:-$PWD}}
  mkdir -p "$legdir"
  if [ -z "$CAPTURE" ]; then
    say "profile: no capture wrapper — diagnostics skipped (never gating)"
    exit 0
  fi
  if [ "$DRY" = 1 ]; then
    say "profile: would capture via $CAPTURE run diagnostic w7-$profile $legdir (dry)"
    exit 0
  fi
  if [ -z "${PERF_W7_PROFILE_CMD:-}" ]; then
    say "profile: PERF_W7_PROFILE_CMD unset (restore seam is lane E's) — diagnostics skipped (never gating)"
    exit 0
  fi
  exec "$CAPTURE" run diagnostic "w7-$profile" "$legdir" -- sh -c "$PERF_W7_PROFILE_CMD"
}

mode=${1:-}
[ -n "$mode" ] || fail "usage: W7.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>"
shift
case "$mode" in
list)
  echo cold-uffd
  echo warm-file
  ;;
oracle)  cmd_oracle "$@" ;;
run)     cmd_run "$@" ;;
profile) cmd_profile "$@" ;;
*) fail "unknown subcommand: $mode" ;;
esac
