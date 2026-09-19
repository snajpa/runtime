#!/usr/bin/env python3
"""W4 single-block runner (perf-regression suite module helper; lane B).

Executes ONE measured migration block: fresh store copy (outside the timed
window), the tool under exact accounting (`measure.py`), a scratch-peak
sampler, and appends the raw sample record to `<out>/samples.jsonl`. No
verdicts - the harness grades. Exit 0 ok / 40 setup error.
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
MEASURE = os.path.join(HERE, "measure.py")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def dir_stats(path):
    total = 0
    objects = 0
    for dp, _, fs in os.walk(path):
        for f in fs:
            fp = os.path.join(dp, f)
            try:
                total += os.path.getsize(fp)
                objects += 1
            except OSError:
                pass
    return total, objects


def pctl(xs, p):
    xs = sorted(xs)
    if not xs:
        return 0.0
    k = (len(xs) - 1) * p
    f = int(k)
    c = min(f + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)


class ScratchSampler(threading.Thread):
    def __init__(self, scratch):
        super().__init__(daemon=True)
        self.scratch = scratch
        self.peak = 0
        self.stop = threading.Event()

    def run(self):
        while not self.stop.is_set():
            try:
                out = subprocess.check_output(["du", "-sb", self.scratch],
                                              stderr=subprocess.DEVNULL)
                val = int(out.split()[0])
                if val > self.peak:
                    self.peak = val
            except Exception:
                pass

            self.stop.wait(0.02)


def summary_and_latency(report):
    counters = {}
    lat = []
    try:
        with open(report) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                rec = json.loads(line)
                if "summary" in rec:
                    counters = rec["summary"]
                elif rec.get("action") == "migrate" and "duration_ms" in rec:
                    lat.append(rec["duration_ms"])
    except (FileNotFoundError, json.JSONDecodeError):
        pass

    return counters, lat


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tool", required=True)
    ap.add_argument("--store", required=True, help="fixture base store (copied)")
    ap.add_argument("--ids", required=True)
    ap.add_argument("--kinds", type=int, default=0)
    ap.add_argument("--out", required=True, help="leg-dir for this block")
    ap.add_argument("--profile", required=True)
    ap.add_argument("--leg", required=True)
    ap.add_argument("--block", required=True)
    ap.add_argument("--compress", default="")
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--tool-mode", choices=["migrate", "reconcile"], default="migrate")
    ap.add_argument("--store-class", default="")
    ap.add_argument("--class", dest="env_class", default="quiet")
    a = ap.parse_args()

    for key in ("tool", "store", "ids", "out"):
        setattr(a, key, os.path.abspath(getattr(a, key)))

    os.makedirs(a.out, exist_ok=True)

    store = os.path.join(a.out, "store")
    scratch = os.path.join(a.out, "scratch")
    shutil.rmtree(store, ignore_errors=True)
    shutil.rmtree(scratch, ignore_errors=True)
    os.makedirs(scratch)

    try:
        subprocess.check_call(["cp", "-r", a.store, store])
    except subprocess.CalledProcessError as err:
        print(f"w4: setup error: cannot copy fixture store: {err}", file=sys.stderr)
        return 40

    tool_sha = sha256_file(a.tool)
    store_bytes, store_objects = dir_stats(a.store)

    report = os.path.join(a.out, "report.json")
    acct = os.path.join(a.out, "account")
    log = os.path.join(a.out, "run.log")

    if a.tool_mode == "reconcile":
        cmd = [a.tool, "-mode", "reconcile", "-verify", "-storage-url", f"file://{store}",
               "-builds-file", a.ids, "-report", report,
               "-concurrency", str(a.concurrency)]
    else:
        cmd = [a.tool, "-storage-url", f"file://{store}", "-builds-file", a.ids,
               "-apply", "-target-header-version", "5", "-scratch-dir", scratch,
               "-report", report, "-concurrency", str(a.concurrency)]
        if a.compress:
            cmd += ["-compress-type", a.compress]

    seed = hashlib.sha256(f"{os.environ.get('PERF_RUN_ID', 'run')}-{a.leg}-{a.block}"
                          .encode()).hexdigest()[:12]

    sampler = ScratchSampler(scratch)
    sampler.start()
    t0 = time.time()
    with open(log, "wb") as f:
        subprocess.call(["python3", MEASURE] + cmd, stdout=f, stderr=subprocess.STDOUT,
                        env=dict(os.environ, TMPDIR=scratch, ACCOUNT_FILE=acct))
    wall = time.time() - t0
    sampler.stop.set()
    sampler.join()

    acc = {}
    try:
        with open(acct) as f:
            for tok in f.read().split():
                k, _, v = tok.partition("=")
                acc[k] = v
    except OSError:
        pass

    counters, lat = summary_and_latency(report)
    expected = 0
    if a.kinds:
        with open(a.ids) as f:
            expected = sum(1 for line in f if line.strip()) * a.kinds

    record = {
        "phase": os.environ.get("PERF_PHASE", "block"),
        "run_id": os.environ.get("PERF_RUN_ID", ""),
        "leg": a.leg,
        "block": a.block,
        "profile": a.profile,
        "tool_mode": a.tool_mode,
        "store_class": a.store_class,
        "class": a.env_class,
        "seed": seed,
        "rc": int(acc.get("rc", -1)),
        "wall_s": float(acc.get("wall", wall)),
        "user_s": float(acc.get("user", 0)),
        "sys_s": float(acc.get("sys", 0)),
        "peak_rss_kib": int(acc.get("maxrss_kb", 0)),
        "scratch_peak_bytes": sampler.peak,
        "tool_sha256": tool_sha,
        "store": {"provider": "file://", "objects": store_objects, "bytes": store_bytes,
                  "header_version": 4},
        "ids": os.path.basename(a.ids),
        "expected_artifacts": expected,
        "cmd": cmd,
        "counters": counters,
    }
    if lat:
        record["latency_ms"] = {
            "n": len(lat),
            "p50": round(pctl(lat, 0.5)),
            "p95": round(pctl(lat, 0.95)),
            "p99": round(pctl(lat, 0.99)),
            "max": max(lat),
        }

    with open(os.path.join(a.out, "samples.jsonl"), "a") as f:
        f.write(json.dumps(record) + "\n")

    shutil.rmtree(store, ignore_errors=True)
    shutil.rmtree(scratch, ignore_errors=True)

    print(f"w4: block {a.block} leg {a.leg} profile {a.profile}: rc={record['rc']} "
          f"wall={record['wall_s']:.3f}s rss_kib={record['peak_rss_kib']} "
          f"scratch={record['scratch_peak_bytes']} counters={counters}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
