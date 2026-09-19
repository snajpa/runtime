#!/usr/bin/env python3
"""perf/2 T1 emitter (lane F) - the only writer to perf-runs.jsonl.

Usage:
  emit.sh append --stream <path> --kind <K> --phase <p> --mode <m>   (body on stdin)
  emit.sh append --run-dir <dir>                (stream = <dir>/perf-runs.jsonl)

Envelope: --run-id/--phase/--mode override env PERF_RUN_ID/PERF_PHASE/PERF_MODE;
PERF_SUITE_REF/PERF_SUITE_SHA fill suite{}. Kind names follow the PASSed
perf/2 schema (env|capability|oracle|resolution|block|sample|artifact|note).

Validates syntax + the perf/2 required fields/enums (vendored minimal check;
perf-2.schema.json vendored alongside); stamps ts; write-then-fsync append
(never a partial line). Exit: 0 ok; 40 setup_error."""
import json
import os
import sys
import time

RECORD_REQUIRED = {
    "env": ["env_class", "host", "tools", "disk_guard"],
    "capability": ["name", "status", "requirement"],
    "oracle": ["kind", "passed"],
    "resolution": ["metric", "sample_count", "bands", "method", "fitted",
                    "discriminability"],
    "block": ["index", "warmup", "order", "namespace", "seed"],
    "sample": ["workload", "profile", "side", "leg", "block", "stage",
               "metric", "unit_id", "chunk", "samples"],
    "artifact": ["kind", "path", "sha256"],
    "note": ["text"],
}
ENUMS = {
    "phase": ["calibrate", "run", "confirm", "diagnose", "report"],
    "mode": ["clean", "confirmation", "diagnostic"],
    "env_class": ["quiet", "busy"],
    "status": ["available", "unavailable", "permission-denied"],
    "requirement": ["to_execute", "validity", "optional"],
    "baseline": ["shared_path", "no_historical_baseline"],
    "order": ["ABBA", "BAAB"],
    "side": ["baseline", "candidate"],
    "leg": ["A", "B", "AA"],
    "stage": ["warmup", "measure"],
    "class": ["cold", "warm"],
    "window": ["first_io", "steady"],
    "op_class": ["read", "write", "modify", "mixed"],
    "scope": ["host", "guest"],
}
PHASES = ENUMS["phase"]
MODES = ENUMS["mode"]


def load_registry():
    p = os.path.join(os.path.dirname(os.path.abspath(__file__)), "registry.json")
    try:
        with open(p, "r", encoding="utf-8") as fh:
            return json.load(fh)
    except Exception:
        return None


def fail(msg):
    print("emit: setup_error: " + msg, file=sys.stderr)
    sys.exit(40)


def main(argv):
    if not argv or argv[0] in ("-h", "--help"):
        print(__doc__)
        return 0
    if argv[0] != "append":
        fail("unknown subcommand %r (expected: append)" % argv[0])
    argv = argv[1:]

    stream = None
    run_dir = None
    kind = None
    phase = None
    mode = None
    run_id = None
    i = 0
    while i < len(argv):
        a = argv[i]
        if a in ("--stream", "--run-dir", "--kind", "--phase", "--mode", "--run-id"):
            i += 1
            if i >= len(argv):
                fail("%s needs a value" % a)
            if a == "--stream":
                stream = argv[i]
            elif a == "--run-dir":
                run_dir = argv[i]
            elif a == "--kind":
                kind = argv[i]
            elif a == "--phase":
                phase = argv[i]
            elif a == "--mode":
                mode = argv[i]
            else:
                run_id = argv[i]
        else:
            fail("unexpected argument: " + a)
        i += 1

    if stream is None and run_dir is not None:
        stream = os.path.join(run_dir, "perf-runs.jsonl")
    if stream is None:
        fail("--stream <path> (or --run-dir <dir>) is required")

    run_id = run_id or os.environ.get("PERF_RUN_ID")
    if not run_id:
        fail("run id missing (--run-id or PERF_RUN_ID)")
    if not (run_id.startswith("run-") and len(run_id) == 19):
        fail("run id malformed: " + run_id)
    phase = phase or os.environ.get("PERF_PHASE", "run")
    mode = mode or os.environ.get("PERF_MODE", "clean")
    if phase not in PHASES:
        fail("phase=%r not in %s" % (phase, PHASES))
    if mode not in MODES:
        fail("mode=%r not in %s" % (mode, MODES))
    suite = {"ref": os.environ.get("PERF_SUITE_REF", ""),
             "sha": os.environ.get("PERF_SUITE_SHA", "")}

    text = sys.stdin.read()
    try:
        rec = json.loads(text)
    except Exception as exc:
        fail("invalid JSON: %s" % exc)
    if not isinstance(rec, dict):
        fail("record must be a JSON object")

    if kind is not None:
        if kind not in RECORD_REQUIRED:
            fail("kind=%r not in %s" % (kind, list(RECORD_REQUIRED)))
        if "record" in rec and rec["record"] != kind:
            fail("kind=%r does not match body record=%r" % (kind, rec["record"]))
        rec["record"] = kind

    rec["schema"] = "perf/2"
    rec["run_id"] = run_id
    rec["phase"] = phase
    rec["mode"] = mode
    rec["suite"] = suite
    rec.setdefault("ts", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))

    rkind = rec.get("record")
    if rkind not in RECORD_REQUIRED:
        fail("record=%r not in %s" % (rkind, list(RECORD_REQUIRED)))
    for key in RECORD_REQUIRED[rkind]:
        if key not in rec:
            fail("missing required field for %s: %s" % (rkind, key))
    for field, allowed in ENUMS.items():
        if field in rec and rec[field] not in allowed:
            fail("%s=%r not in %s" % (field, rec[field], allowed))
    if rkind == "sample" and not isinstance(rec.get("samples"), list):
        fail("samples must be a list (raw sample records are mandatory)")
    if rkind == "sample":
        reg = load_registry()
        if reg is not None:
            mid = (rec.get("metric") or {}).get("id")
            known = {m["id"] for m in reg.get("metrics", [])}
            known |= {m["id"] for m in reg.get("proposed", [])}
            if mid not in known:
                fail("unknown metric id %r - register it in schema/registry.json" % mid)
            unit = (rec.get("metric") or {}).get("unit")
            for m in reg.get("metrics", []):
                if m["id"] == mid and unit and m.get("unit") and m["unit"] != unit:
                    print("emit: warning: unit %r differs from registry %r for %s"
                          % (unit, m.get("unit"), mid), file=sys.stderr)

    line = json.dumps(rec, separators=(",", ":"), sort_keys=True) + "\n"
    try:
        fd = os.open(stream, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
        try:
            os.write(fd, line.encode("utf-8"))
            os.fsync(fd)
        finally:
            os.close(fd)
    except OSError as exc:
        fail("append failed: %s" % exc)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
