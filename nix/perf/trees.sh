#!/bin/sh
# trees.sh — materialize/validate the baseline+candidate trees (perf-regression suite; lane A).
# Usage:
#   trees.sh materialize <label> <ref> [outdir]   # worktree + build metadata + smoke build
#   trees.sh validate <label>                     # clean worktree at its recorded head?
# Worktree: ${PERF_TREE_DIR:-${PERF_TREES_DIR:-/root/ai/worktrees/e2b}/perf-<label>}
# Records:  <outdir>/tree-<label>-meta.txt (default $HOME/ai/logs/e2b-perf/tree-<label>)
# Exit: materialize passes the smoke-build rc; validate 0 ok / 40 mismatch.
set -u
cmd=${1:?usage: trees.sh materialize <label> <ref> [outdir] | validate <label>}
shift
trees_dir=${PERF_TREES_DIR:-/root/ai/worktrees/e2b}
case "$cmd" in
materialize)
  label=${1:?usage: trees.sh materialize <label> <ref> [outdir]}
  ref=${2:?usage: trees.sh materialize <label> <ref> [outdir]}
  outdir=${3:-${PERF_LOG_ROOT:-$HOME/ai/logs/e2b-perf}/tree-$label}
  repo=${PERF_REPO:-$(git rev-parse --show-toplevel 2>/dev/null || echo /root/ai/worktrees/e2b/exp-slave1)}
  wt=${PERF_TREE_DIR:-$trees_dir/perf-$label}
  mkdir -p "$outdir"
  if [ ! -d "$wt" ]; then git -C "$repo" worktree add --detach "$wt" "$ref" || exit 40; fi
  git -C "$wt" checkout --detach "$ref" || exit 40
  git -C "$wt" reset --hard >/dev/null || exit 40
  git -C "$wt" clean -fdx >/dev/null
  sha=$(git -C "$wt" rev-parse HEAD)
  out="$outdir/tree-$label-meta.txt"
  {
    echo "label: $label"
    echo "ref: $ref"
    echo "sha: $sha"
    echo "describe: $(git -C "$wt" describe --tags --always 2>/dev/null || echo n/a)"
    echo "dirty_files: $(git -C "$wt" status --porcelain | wc -l)"
    echo "go_version: $(cd "$wt" && go version 2>/dev/null)"
    echo "tool_versions_sha: $(sha256sum "$wt/.tool-versions" 2>/dev/null | cut -d' ' -f1)"
    fl=$(sha256sum "$wt/flake.lock" 2>/dev/null | cut -d' ' -f1); echo "flake_lock_sha: ${fl:-n/a}"
    echo "captured_at_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "$out"
  cl=""
  if [ -e "$wt/flake.nix" ]; then
    cl=$(cd "$wt" && timeout 90 nix path-info --json -r .#dev-vm 2>/dev/null | sha256sum | cut -d' ' -f1)
  fi
  echo "nix_closure_dev_vm_sha: ${cl:-n/a}" >> "$out"
  build_cmd=${PERF_BUILD_CMD:-"./packages/orchestrator/pkg/sandbox/block"}
  if (cd "$wt" && go build $build_cmd); then rc=0; else rc=$?; fi
  echo "build_cmd: $build_cmd" >> "$out"
  echo "build_rc: $rc" >> "$out"
  gw=$(sha256sum "$wt/go.work.sum" 2>/dev/null | cut -d' ' -f1); echo "go_work_sum_sha_postbuild: ${gw:-n/a}" >> "$out"
  cat "$out"
  exit $rc
  ;;
validate)
  label=${1:?usage: trees.sh validate <label>}
  wt=${PERF_TREE_DIR:-$trees_dir/perf-$label}
  [ -d "$wt" ] || { echo "validate: no worktree: $wt"; exit 40; }
  d=$(git -C "$wt" status --porcelain | wc -l)
  echo "tree $label: $(git -C "$wt" rev-parse HEAD) dirty=$d"
  [ "$d" = 0 ] || exit 40
  ;;
*) echo "usage: trees.sh materialize <label> <ref> [outdir] | validate <label>" >&2; exit 2 ;;
esac
