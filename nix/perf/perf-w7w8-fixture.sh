#!/bin/sh
# W7/W8 snapshot fixture recorder (lane A) — RE-LAND DRAFT (NOT landed; wave freeze).
# Purpose: produce <fixture-dir>/snapshot.json — the section 11.1 identity block consumed by
# modules/W7.sh + modules/W8.sh (their `oracle` emits it as the `snapshot` record) — plus the
# quiesced marker it links. Writes are atomic (tmp + rename); every missing input aborts (40);
# no snapshot.json is ever written partially.
# Usage:
#   perf-w7w8-fixture.sh create   # run the creation seam (confirm with E/agent0), then record
#   perf-w7w8-fixture.sh record   # (re)record identity from an existing fixture dir
#   perf-w7w8-fixture.sh verify   # validate an existing snapshot.json against the 11.1 set
# Required env:
#   PERF_W7W8_FIXTURE_DIR      fixture dir holding the paused-snapshot artifacts
#   PERF_W7W8_CLASS            cold | warm (immutable; cold == UFFD-lazy, warm == file-backed)
#   PERF_W7W8_RESET_PROCEDURE  JSON: predeclared cache/reset procedure for the class
#   PERF_W7W8_CACHE_EVIDENCE   JSON: objective pre-run cache-state evidence for the class
# Creation seam (create mode; lane E/agent0 confirmation pending):
#   PERF_W7W8_CREATE_CMD       run in $PERF_W7W8_FIXTURE_DIR; rc 0; must leave the paused
#                              snapshot artifacts (resume-build -cmd-pause; exact invocation TBC)
#   PERF_W7W8_MARKER_CMD       run after CREATE_CMD; writes marker.json {sha256,state,...}; rc 0
# Optional:
#   PERF_W7W8_SNAPSHOT_ID      explicit id (default: snap-<first 12 of the content digest>)
#   PERF_W7W8_MANIFEST         "<role> <path>" lines; default scans the known artifact names
#   PERF_W7W8_TREE_DIR         source tree (default /root/ai/worktrees/e2b/perf-candidate)
#   PERF_W7W8_OBJECTSTORE      object-store state token (default "empty")
# Recon note (2026-09-19): the paused snapshot's local artifacts are the `snapfile` blob plus
# memfile/rootfs/metadata (packages/shared/pkg/storage/paths.go: SnapfileName, Paths.Snapfile;
# orchestrator pkg/sandbox/snapshot.go: Snapfile; sandbox Pause -> cachePaths.CacheSnapfile()).
set -u

FIX=${PERF_W7W8_FIXTURE_DIR:-}
CLASS=${PERF_W7W8_CLASS:-}
MODE=${1:-}
TREE=${PERF_W7W8_TREE_DIR:-/root/ai/worktrees/e2b/perf-candidate}
KNOWN="snapfile memfile memfile.header rootfs metadata fcversion config.json"

say() { echo "+ $*" >&2; }
fail() { echo "w7w8-fixture: $*" >&2; exit 40; }

case "$MODE" in create|record|verify) ;; *) fail "usage: perf-w7w8-fixture.sh create|record|verify" ;; esac
[ -n "$FIX" ] || fail "PERF_W7W8_FIXTURE_DIR unset"
case "$CLASS" in cold|warm) ;; *) fail "PERF_W7W8_CLASS must be cold|warm" ;; esac

write_manifest() { # -> "$FIX/manifest.txt"
  m="$FIX/manifest.txt"
  if [ -n "${PERF_W7W8_MANIFEST:-}" ]; then
    [ -f "$PERF_W7W8_MANIFEST" ] || fail "PERF_W7W8_MANIFEST missing: $PERF_W7W8_MANIFEST"
    cp -- "$PERF_W7W8_MANIFEST" "$m"
  else
    : >"$m"
    for name in $KNOWN; do
      [ -f "$FIX/$name" ] && printf '%s %s\n' "$name" "$FIX/$name" >>"$m"
    done
    [ -s "$m" ] || fail "no snapshot artifacts in $FIX (set PERF_W7W8_MANIFEST)"
  fi
  echo "$m"
}

content_digest() { # $1=manifest -> digest over sorted "<role>:<sha256>" lines
  tmp=$(mktemp) || fail "mktemp failed"
  while read -r role path; do
    [ -n "${role:-}" ] || continue
    [ -f "$path" ] || fail "manifest artifact missing: $path"
    h=$(sha256sum "$path" | awk '{print $1}')
    printf '%s:%s\n' "$role" "$h" >>"$tmp"
  done <"$1"
  sort -o "$tmp" "$tmp"
  sha256sum "$tmp" | awk '{print $1}'
  rm -f "$tmp"
}

record() {
  [ -d "$FIX" ] || fail "fixture dir missing: $FIX (run create first)"
  [ -f "$FIX/marker.json" ] || fail "quiesced marker missing: $FIX/marker.json (create / MARKER_CMD)"
  [ -f "$FIX/config.json" ] || fail "VM/FC config missing: $FIX/config.json (creation seam must record it)"
  [ -n "${PERF_W7W8_RESET_PROCEDURE:-}" ] || fail "PERF_W7W8_RESET_PROCEDURE unset — predeclared per class"
  [ -n "${PERF_W7W8_CACHE_EVIDENCE:-}" ] || fail "PERF_W7W8_CACHE_EVIDENCE unset — predeclared per class"
  m=$(write_manifest)
  digest=$(content_digest "$m")
  sid=${PERF_W7W8_SNAPSHOT_ID:-snap-$(printf '%s' "$digest" | cut -c1-12)}
  marker=$(cat "$FIX/marker.json")
  python3 - "$FIX" "$sid" "$digest" "$CLASS" "$TREE" "$marker" \
      "$PERF_W7W8_RESET_PROCEDURE" "$PERF_W7W8_CACHE_EVIDENCE" "${PERF_W7W8_OBJECTSTORE:-empty}" <<'PY' || fail "snapshot.json assembly failed"
import json, os, subprocess, sys
fix, sid, digest, cls, tree, marker_s, reset_s, cache_s, store = sys.argv[1:10]
def git(*a):
    return subprocess.run(["git", "-C", tree] + list(a), capture_output=True, text=True).stdout.strip()
cfg = json.load(open(fix + "/config.json"))
rec = {
    "snapshot_id": sid,
    "content_sha256": digest,
    "source_tree": {"path": tree, "head": git("rev-parse", "HEAD"), "describe": git("describe", "--always", "--dirty")},
    "firecracker": {"version": cfg.get("firecracker", cfg.get("fcversion", ""))},
    "kernel": cfg.get("kernel", {}),
    "rootfs": cfg.get("rootfs", {}),
    "init": cfg.get("init", {}),
    "config": cfg.get("config", {}),
    "vcpu": int(cfg.get("vcpu", 0)),
    "memory_mib": int(cfg.get("memory_mib", 0)),
    "cpu_template": cfg.get("cpu_template", ""),
    "network": cfg.get("network", {}),
    "ports": cfg.get("ports", []),
    "uffd": {"mode": ("lazy" if cls == "cold" else "file"), "backing": "file"},
    "object_store_state": {"status": store},
    "restore_state_dir": fix,
    "quiesced_marker": json.loads(marker_s),
    "class": cls,
    "cache_state_evidence": json.loads(cache_s),
    "reset_procedure": json.loads(reset_s),
}
tmp = fix + "/snapshot.json.tmp"
with open(tmp, "w") as f:
    json.dump(rec, f, sort_keys=True, indent=1)
    f.flush()
    os.fsync(f.fileno())
os.replace(tmp, fix + "/snapshot.json")
print("wrote %s/snapshot.json" % fix)
PY
  verify
}

verify() {
  [ -f "$FIX/snapshot.json" ] || fail "snapshot.json missing: $FIX/snapshot.json"
  python3 - "$FIX/snapshot.json" <<'PY' || fail "snapshot.json incomplete"
import json, sys
req = ["snapshot_id", "content_sha256", "source_tree", "firecracker", "kernel", "rootfs",
       "init", "config", "vcpu", "memory_mib", "cpu_template", "network", "ports", "uffd",
       "object_store_state", "restore_state_dir", "quiesced_marker", "class",
       "cache_state_evidence", "reset_procedure"]
rec = json.load(open(sys.argv[1]))
missing = [k for k in req if rec.get(k) in (None, "", {}, [])]
if missing:
    print("incomplete: %s" % ", ".join(missing), file=sys.stderr)
    sys.exit(40)
print("ok: snapshot.json complete (id=%s class=%s)" % (rec["snapshot_id"], rec["class"]))
PY
}

case "$MODE" in
create)
  [ -n "${PERF_W7W8_CREATE_CMD:-}" ] || fail "PERF_W7W8_CREATE_CMD unset (creation seam to confirm)"
  [ -n "${PERF_W7W8_MARKER_CMD:-}" ] || fail "PERF_W7W8_MARKER_CMD unset (marker seam to confirm)"
  mkdir -p "$FIX"
  say "create: running the creation seam in $FIX"
  ( cd "$FIX" && sh -c "$PERF_W7W8_CREATE_CMD" ) || fail "creation seam failed"
  say "create: writing the quiesced marker"
  ( cd "$FIX" && sh -c "$PERF_W7W8_MARKER_CMD" ) || fail "marker seam failed"
  record
  ;;
record) record ;;
verify) verify ;;
esac
