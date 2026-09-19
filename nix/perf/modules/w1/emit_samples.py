#!/usr/bin/env python3
"""W1 fio JSON -> perf/2 sample bodies (lane D; raw producer only).

Reads the VM leg artifacts copied back into --vm-dir (fio.json + meta.json,
as written by the W1 bench driver) and prints perf/2 record bodies to stdout,
one JSON per line, for the harness's feed_leg to forward to the T1 emitter.

Exit codes (module scheme):
  0  ok, records emitted
  30 measurement failure (bench meta ok=false, class "measurement"/"integrity",
     or fio output missing/unusable) — the cell gets no verdict
  40 setup failure (meta missing / class "setup" / bad invocation)
"""
import argparse
import hashlib
import json
import os
import sys

# profile direction mapping lives in w1.sh; the parser reads the direction
# sections of the fio JSON that actually saw I/O.
DIRECTIONS = ("read", "write", "trim")


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--vm-dir", required=True)
    ap.add_argument("--legdir", required=True)
    ap.add_argument("--workload", default="W1")
    ap.add_argument("--profile", required=True)
    ap.add_argument("--side", required=True)
    ap.add_argument("--leg", required=True)
    ap.add_argument("--block", type=int, required=True)
    ap.add_argument("--stage", required=True)
    ap.add_argument("--host-loadavg", default="")
    ap.add_argument("--class", dest="env_class", default="quiet")
    args = ap.parse_args()

    meta_path = os.path.join(args.vm_dir, "meta.json")
    if not os.path.isfile(meta_path):
        print("w1: no meta.json in %s (bench did not report)" % args.vm_dir, file=sys.stderr)
        return 40

    try:
        with open(meta_path, "r", encoding="utf-8") as fh:
            meta = json.load(fh)
    except Exception as exc:
        print("w1: meta.json unreadable: %s" % exc, file=sys.stderr)
        return 40

    def emit(rec):
        sys.stdout.write(json.dumps(rec, separators=(",", ":")) + "\n")

    def note(text):
        emit({"record": "note", "text": text})

    def artifacts():
        for kind, name in (("fio-json", "fio.json"), ("bench-meta", "meta.json"),
                           ("fio-job", "fio.job")):
            path = os.path.join(args.vm_dir, name)
            if os.path.isfile(path):
                emit({"record": "artifact", "kind": kind, "path": os.path.abspath(path),
                      "sha256": sha256(path), "retain": True})

    artifacts()

    note("w1 leg covariates: class=%s host_loadavg=%s guest_loadavg=%s->%s kernel=%s fio=%s device=%s backend_bytes=%s" % (
        args.env_class, args.host_loadavg or "n/a",
        meta.get("loadavg_before", "n/a"), meta.get("loadavg_after", "n/a"),
        meta.get("kernel", "n/a"), meta.get("fio_version", "n/a"),
        meta.get("device", "n/a"), meta.get("backend_bytes", "n/a")))

    if not meta.get("ok"):
        cls = meta.get("error_class", "")
        note("w1 bench failed (%s): %s" % (cls or "unknown", str(meta.get("error", ""))[:500]))
        return 40 if cls == "setup" else 30

    fio_path = os.path.join(args.vm_dir, "fio.json")
    if not os.path.isfile(fio_path):
        note("w1 fio.json missing after an ok bench")
        return 30

    try:
        with open(fio_path, "r", encoding="utf-8") as fh:
            fio = json.load(fh)
        job = (fio.get("jobs") or [])[0]
    except Exception as exc:
        note("w1 fio.json unusable: %s" % exc)
        return 30

    if job.get("error") not in (None, 0):
        note("w1 fio job error=%s" % job.get("error"))
        return 30

    def sample(mid, unit, unit_id, value):
        emit({"record": "sample", "workload": args.workload, "profile": args.profile,
              "side": args.side, "leg": args.leg, "block": args.block, "stage": args.stage,
              "metric": {"id": mid, "unit": unit}, "unit_id": unit_id,
              "chunk": {"i": 1, "n": 1}, "samples": [round(float(value), 6)]})

    def pct(section, p):
        # clat is the completion latency; fio reports the fsync/flush data
        # (the `sync` section) under lat_ns only.
        for key in ("clat_ns", "lat_ns"):
            v = ((section.get(key) or {}).get("percentile") or {}).get(p)
            if v is not None:
                return v
        return None

    total_ios = 0.0
    emitted = 0

    for direction in DIRECTIONS:
        sec = job.get(direction) or {}
        ios = sec.get("total_ios") or 0
        if not ios:
            continue
        total_ios += float(ios)

        sample("throughput_mib_s", "MiB/s", direction,
               (sec.get("bw_bytes") or 0) / 1048576.0)
        sample("iops", "count/s", direction, sec.get("iops") or 0)

        for mid, key in (("lat_p50_us", "50.000000"), ("lat_p95_us", "95.000000"),
                         ("lat_p99_us", "99.000000"), ("lat_p999_us", "99.900000")):
            v = pct(sec, key)
            if v is not None:
                sample(mid, "us", direction, float(v) / 1000.0)

        if (sec.get("clat_ns") or {}).get("max") is not None:
            sample("lat_max_us", "us", direction, float(sec["clat_ns"]["max"]) / 1000.0)

        emitted += 1

    if total_ios:
        usr = job.get("usr_cpu") or 0
        sys_cpu = job.get("sys_cpu") or 0
        runtime_ms = job.get("job_runtime") or 0
        cpu_s = (usr + sys_cpu) / 100.0 * (runtime_ms / 1000.0)
        sample("cpu_us_per_op", "us/op", "job", cpu_s * 1e6 / total_ios)

    if job.get("ctx") is not None:
        sample("ctx_switches", "count", "job", job.get("ctx"))

    sync = job.get("sync") or {}
    if sync.get("total_ios"):
        v = pct(sync, "50.000000")
        if v is not None:
            sample("fsync_latency_ms", "ms", "sync", float(v) / 1e6)

    if not emitted:
        note("w1 no direction saw I/O in %s" % fio_path)
        return 30

    return 0


if __name__ == "__main__":
    sys.exit(main())
