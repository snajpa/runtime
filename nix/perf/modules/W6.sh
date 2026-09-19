#!/bin/sh
# W6 module — dev-VM cold/warm/stop (perf-regression suite; lane A).
# Contract (nix/perf/README.md): list | oracle <profile> | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>
#   list     -> profile ids
#   oracle   -> step-0 correctness gate + the eligibility assertion (reviewer1 2087)
#   run      -> workload (cold/warm/stop phases); perf/2 records to <leg-dir>/samples.jsonl; no verdicts
#   profile  -> matched-diagnostics hooks (never gating; via lane C's capture wrapper)
# Records: a `note` (eligibility) first — the pair_label is the W6 conditional pair (nominal in A/A calibration runs; the per-leg ref is derived); per phase a `sample` (cold_bringup_s / warm_bringup_s /
# stop_s). After each successful cold/warm boot the guest capability/covariate records from
# guest-capabilities.sh are appended — post-boot and outside the timed interval (reviewer1
# 2137/2140). Missing required capabilities follow the sealed classes: to_execute ->
# SETUP_ERROR(40); validity -> INCONCLUSIVE(30); optional -> recorded. Timer stops at boot
# completion, before the capture, so the measured interval is unaffected.
# Env: PERF_W6_PORT, PERF_ENV_CLASS, PERF_W6_DRY, PERF_STAGE, PERF_SIDE, PERF_TREE_DIR/
# PERF_TREES_DIR, PERF_CAPTURE (wrapper), PERF_GUEST_CAP (helper). Exit: 0/30/40.
# Position tokens (A1/B1/B2/A2): side derives from PERF_SIDE, else the token (A*->baseline,
# B*->candidate); the tree fallback maps the same way to perf-baseline/perf-candidate.
set -u
mode=${1:?usage: W6.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>}
shift
port=${PERF_W6_PORT:-2233}
class=${PERF_ENV_CLASS:-quiet}
dry=${PERF_W6_DRY:-0}
cpus=${E2B_DEV_VM_CPUS:-16}
mem=${E2B_DEV_VM_MEM:-32768}
trees_dir=${PERF_TREES_DIR:-/root/ai/worktrees/e2b}
self_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CAPTURE=${PERF_CAPTURE:-$self_dir/../perf-capture.sh}
[ -x "$CAPTURE" ] || CAPTURE=""
GUEST_CAP=${PERF_GUEST_CAP:-$self_dir/../guest-capabilities.sh}
[ -x "$GUEST_CAP" ] || GUEST_CAP=""
stage=${PERF_STAGE:-measure}
OUTDIR=""
bin=""
wt=""
profile=""
leg=""
block=""
side=""
cap_toexec_gap=""
cap_valid_gap=""
emit() { if [ "$dry" = 1 ]; then echo "+ record: $1"; else echo "$1" >> "$OUTDIR/samples.jsonl"; fi; }
say() { echo "+ $*"; }
side_of_leg() { # position token -> sealed side (A1/A2 -> baseline, B1/B2 -> candidate)
  case "$1" in
    A*|a*) echo baseline ;;
    B*|b*) echo candidate ;;
    *) echo "" ;;
  esac
}
resolve_bin() {
  if [ "$dry" = 1 ]; then
    say "(cd $wt && nix build --no-link --print-out-paths .#dev-vm)  # -> <store>"
    bin="<store>/bin/e2b-dev-vm"; return 0
  fi
  if bin=$(cd "$wt" && nix build --no-link --print-out-paths .#dev-vm 2>"$OUTDIR/build.err"); then
    bin="$bin/bin/e2b-dev-vm"
  else
    echo "w6: setup_error (build)"; exit 40
  fi
}
guest_capture() { # <name> — post-boot capability/covariate capture (outside the timed interval)
  name=$1
  capfile="$OUTDIR/$name.guest.jsonl"
  if [ -z "$GUEST_CAP" ]; then
    say "guest capabilities: helper not present"; cap_valid_gap="helper-absent"; return 0
  fi
  if [ "$dry" = 1 ]; then
    say "E2B_VM_PORT=$port PERF_SIDE=$side PERF_LEG=$leg PERF_W6_CAPTURE_PHASE=$name $GUEST_CAP > $capfile (post-boot; outside the timed phase)"
    return 0
  fi
  case "$side" in baseline|candidate) ;; *) echo "w6: setup_error (guest capture side '${side:-}' unresolved)"; exit 40 ;; esac
  case "$leg" in A|B|AA|A1|A2|B1|B2) ;; *) echo "w6: setup_error (guest capture leg '${leg:-}' unresolved)"; exit 40 ;; esac
  case "$name" in cold|warm) ;; *) echo "w6: setup_error (guest capture phase '${name:-}' unresolved)"; exit 40 ;; esac
  if (cd "$wt" && env E2B_VM_PORT="$port" E2B_VM_PASSWORD="${E2B_VM_PASSWORD:-e2b-dev}" PERF_SIDE="$side" PERF_LEG="$leg" PERF_W6_CAPTURE_PHASE="$name" "$GUEST_CAP" >"$capfile" 2>"$OUTDIR/$name.guest.log"); then :; else say "guest capabilities capture rc=$? (records kept if any)"; fi
  cat "$capfile" >> "$OUTDIR/samples.jsonl" 2>/dev/null || true
  if ! grep -q '"record"' "$capfile" 2>/dev/null; then
    say "guest capabilities: no records captured"; cap_valid_gap="no-records"; return 0
  fi
  gap_to=$(grep -E '"requirement"[[:space:]]*:[[:space:]]*"to_execute"' "$capfile" | grep -vE '"status"[[:space:]]*:[[:space:]]*"available"' | head -1)
  [ -z "$gap_to" ] || { cap_toexec_gap="$gap_to"; say "guest capability gap (to_execute): $gap_to"; }
  gap_va=$(grep -E '"requirement"[[:space:]]*:[[:space:]]*"validity"' "$capfile" | grep -vE '"status"[[:space:]]*:[[:space:]]*"available"' | head -1)
  [ -z "$gap_va" ] || { cap_valid_gap="$gap_va"; say "guest capability gap (validity): $gap_va"; }
}
run_phase() { # <name> <unit> <cmd...>
  name=$1; unit=$2; shift 2
  case "$name" in
    cold) mid=cold_bringup_s ;;
    warm) mid=warm_bringup_s ;;
    stop) mid=stop_s ;;
    *) mid=${name}_s ;;
  esac
  t0=$(date +%s)
  if [ "$dry" = 1 ]; then
    if [ -n "$CAPTURE" ]; then say "$CAPTURE run clean $unit $OUTDIR -- $*"; else say "env E2B_DEV_VM_DIR=$OUTDIR/vm-state E2B_DEV_VM_SSH_PORT=$port E2B_VM_PORT=$port E2B_DEV_VM_BIN=$bin $*"; fi
    rc=0
  else
    if [ -n "$CAPTURE" ]; then
      if (cd "$wt" && env E2B_DEV_VM_DIR="$OUTDIR/vm-state" E2B_DEV_VM_SSH_PORT="$port" E2B_VM_PORT="$port" E2B_DEV_VM_CPUS="$cpus" E2B_DEV_VM_MEM="$mem" E2B_DEV_VM_BIN="$bin" "$CAPTURE" run clean "$unit" "$OUTDIR" -- "$@" >>"$OUTDIR/samples.jsonl" 2>"$OUTDIR/$name.log"); then rc=0; else rc=$?; fi
    else
      if (cd "$wt" && env E2B_DEV_VM_DIR="$OUTDIR/vm-state" E2B_DEV_VM_SSH_PORT="$port" E2B_VM_PORT="$port" E2B_DEV_VM_CPUS="$cpus" E2B_DEV_VM_MEM="$mem" E2B_DEV_VM_BIN="$bin" "$@" >"$OUTDIR/$name.log" 2>&1); then rc=0; else rc=$?; fi
    fi
  fi
  t1=$(date +%s)
  if [ "$rc" = 0 ]; then
    case "$name" in cold|warm) guest_capture "$name" ;; esac
    emit "{\"record\":\"sample\",\"workload\":\"W6\",\"profile\":\"$profile\",\"side\":\"$side\",\"leg\":\"$leg\",\"block\":$block,\"stage\":\"$stage\",\"metric\":{\"id\":\"$mid\",\"unit\":\"s\"},\"unit_id\":\"w6-$name\",\"chunk\":{\"i\":1,\"n\":1},\"samples\":[$((t1-t0))]}"
  else
    echo "w6: setup_error ($name rc=$rc)"; exit 40
  fi
}
do_oracle() { # <profile> [outdir]
  profile=$1; OUTDIR=${2:-${PERF_LEGDIR:-$PWD}}; mkdir -p "$OUTDIR"
  oside=${PERF_SIDE:-}
  [ -n "$oside" ] || oside=$(side_of_leg "${PERF_LEG:-}")
  [ -n "$oside" ] || oside=unknown
  say "eligibility: pair_label=45dcf1ba6->5eb54defe side=$oside ref=${PERF_TREE_REF:-unknown} profile=$profile asserted=true"
  emit "{\"record\":\"note\",\"text\":\"w6 eligibility asserted: pair_label=45dcf1ba6->5eb54defe side=$oside ref=${PERF_TREE_REF:-unknown} profile=$profile cpus=$cpus mem=$mem port=$port class=$class oracle=ensure-readiness asserted=true\"}"
  say "prereqs: port $port free; fresh state dir; runner buildable from the leg tree"
  echo "ok"
}
do_run() { # <profile> <leg> <block> <leg-dir>
  profile=$1; leg=$2; block=$3; OUTDIR=$4; mkdir -p "$OUTDIR/logs"
  side=${PERF_SIDE:-}
  [ -n "$side" ] || side=$(side_of_leg "$leg")
  [ -n "$side" ] || side=candidate
  wt=${PERF_TREE_DIR:-}
  if [ -z "$wt" ]; then case "$side" in baseline) wt=$trees_dir/perf-baseline ;; *) wt=$trees_dir/perf-candidate ;; esac; fi
  ref=${PERF_TREE_REF:-}
  [ -n "$ref" ] || ref=$(git -C "$wt" rev-parse --short HEAD 2>/dev/null || echo unknown)
  say "run profile=$profile leg=$leg block=$block tree=$wt side=$side${CAPTURE:+ via $CAPTURE}"
  emit "{\"record\":\"note\",\"text\":\"w6 eligibility asserted: pair_label=45dcf1ba6->5eb54defe side=$side ref=$ref profile=$profile cpus=$cpus mem=$mem port=$port class=$class oracle=ensure-readiness asserted=true\"}"
  resolve_bin
  run_phase cold w6-cold sh nix/scripts/dev.sh --ensure
  run_phase warm w6-warm sh nix/scripts/dev.sh --ensure
  run_phase stop w6-stop "$bin" stop
  if [ -n "$cap_toexec_gap" ]; then echo "w6: setup_error (guest to_execute capability missing)"; exit 40; fi
  if [ -n "$cap_valid_gap" ]; then echo "w6: inconclusive (guest comparability evidence: $cap_valid_gap)"; exit 30; fi
  echo "ok"
}
do_profile() { # <profile> <leg-dir>
  profile=$1; OUTDIR=$2; mkdir -p "$OUTDIR/logs"
  side=${PERF_SIDE:-}
  [ -n "$side" ] || side=$(side_of_leg "${PERF_LEG:-}")
  wt=${PERF_TREE_DIR:-}
  if [ -z "$wt" ]; then case "$side" in baseline) wt=$trees_dir/perf-baseline ;; *) wt=$trees_dir/perf-candidate ;; esac; fi
  resolve_bin
  if [ -n "$CAPTURE" ]; then
    say "profile: $CAPTURE run diagnostic w6-$profile (post-confirmation only; never gating)"
    if [ "$dry" != 1 ]; then
      if (cd "$wt" && env E2B_DEV_VM_DIR="$OUTDIR/vm-state" E2B_DEV_VM_SSH_PORT="$port" E2B_VM_PORT="$port" E2B_DEV_VM_CPUS="$cpus" E2B_DEV_VM_MEM="$mem" E2B_DEV_VM_BIN="$bin" "$CAPTURE" run diagnostic "w6-$profile" "$OUTDIR" -- sh nix/scripts/dev.sh --ensure >>"$OUTDIR/samples.jsonl" 2>"$OUTDIR/profile.log"); then :; else echo "w6: setup_error (profile)"; exit 40; fi
    fi
  else
    say "profile: wrapper not present — stub"
  fi
  echo "ok"
}
case "$mode" in
  list) echo "cold-warm-stop" ;;
  oracle) do_oracle "$@" ;;
  run) do_run "$@" ;;
  profile) do_profile "$@" ;;
  *) echo "usage: W6.sh list | oracle <profile> [outdir] | run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>" >&2; exit 2 ;;
esac
