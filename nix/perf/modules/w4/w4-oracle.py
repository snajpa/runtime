#!/usr/bin/env python3
"""W4 oracle (step-0 correctness leg) - `migrate-builds` + `reconcile` on a store copy.

Runs before any measured cell (design ruling 7: correctness oracle first). Produces
the correctness evidence (raw counters + logs; no verdicts) and a pass/fail signal.

Exit: 0 oracle passed; 30 oracle assertion failed (the cell cannot produce a perf
verdict -> INCONCLUSIVE, never PASS/FAIL); 40 setup error (missing tool/store, or
the tool failed to execute).

usage: w4-oracle.py --tool <bin> --store <base-store-dir> --ids <builds.txt>
                    --kinds <n> --out <dir> [--mode header|zstd]
"""
import argparse
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
MEASURE = os.path.join(HERE, "measure.py")


def run_tool(args, account, log):
    env = dict(os.environ, ACCOUNT_FILE=account)
    with open(log, "wb") as f:
        subprocess.call(["python3", MEASURE] + args, stdout=f,
                        stderr=subprocess.STDOUT, env=env)
    try:
        with open(account) as f:
            toks = f.read().split()

        return int(next(t.split("=", 1)[1] for t in toks if t.startswith("rc=")))
    except (OSError, StopIteration, ValueError):
        return -1


def summary(report):
    try:
        with open(report) as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                rec = json.loads(line)
                if "summary" in rec:
                    return rec["summary"]
    except (FileNotFoundError, json.JSONDecodeError):
        return None

    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tool", required=True)
    ap.add_argument("--store", required=True)
    ap.add_argument("--ids", required=True)
    ap.add_argument("--kinds", type=int, required=True,
                    help="kinds per build in the fixture (tiny/latency=2, large=1)")
    ap.add_argument("--out", required=True)
    ap.add_argument("--mode", choices=["header", "zstd"], default="header")
    ap.add_argument("--keep-store", action="store_true",
                    help="keep the oracle's store copy (debugging)")
    a = ap.parse_args()
    a.tool = os.path.abspath(a.tool)
    a.store = os.path.abspath(a.store)
    a.ids = os.path.abspath(a.ids)
    a.out = os.path.abspath(a.out)

    for path in (a.tool, a.store, a.ids):
        if not os.path.exists(path):
            print(f"w4-oracle: setup error: missing {path}", file=sys.stderr)
            return 40

    expected = sum(1 for line in open(a.ids) if line.strip()) * a.kinds
    os.makedirs(a.out, exist_ok=True)

    store = os.path.join(a.out, "oracle-store")
    scratch = os.path.join(a.out, "oracle-scratch")
    shutil.rmtree(store, ignore_errors=True)
    shutil.rmtree(scratch, ignore_errors=True)
    os.makedirs(scratch)
    subprocess.check_call(["cp", "-r", a.store, store])

    extra = ["-compress-type", "zstd"] if a.mode == "zstd" else []

    m_report = os.path.join(a.out, "oracle-migrate.report.json")
    rc = run_tool([a.tool, "-storage-url", f"file://{store}", "-builds-file", a.ids,
                   "-apply", "-target-header-version", "5", "-scratch-dir", scratch,
                   "-report", m_report, "-concurrency", "1"] + extra,
                  os.path.join(a.out, "oracle-migrate.acct"),
                  os.path.join(a.out, "oracle-migrate.log"))
    m_sum = summary(m_report)
    if rc != 0 or m_sum is None:
        print(f"w4-oracle: setup error: migrate rc={rc} summary={m_sum}", file=sys.stderr)
        if not a.keep_store:
            shutil.rmtree(store, ignore_errors=True)
            shutil.rmtree(scratch, ignore_errors=True)
        return 40

    r_report = os.path.join(a.out, "oracle-reconcile.report.json")
    rrc = run_tool([a.tool, "-mode", "reconcile", "-verify",
                    "-storage-url", f"file://{store}", "-builds-file", a.ids,
                    "-report", r_report, "-concurrency", "1"],
                   os.path.join(a.out, "oracle-reconcile.acct"),
                   os.path.join(a.out, "oracle-reconcile.log"))
    r_sum = summary(r_report)
    if rrc != 0 or r_sum is None:
        print(f"w4-oracle: setup error: reconcile rc={rrc} summary={r_sum}", file=sys.stderr)
        if not a.keep_store:
            shutil.rmtree(store, ignore_errors=True)
            shutil.rmtree(scratch, ignore_errors=True)
        return 40

    rec = {
        "oracle": "w4",
        "mode": a.mode,
        "expected_artifacts": expected,
        "migrate": m_sum,
        "reconcile": r_sum,
    }
    with open(os.path.join(a.out, "oracle.json"), "w") as f:
        json.dump(rec, f, indent=2)
        f.write("\n")

    migrated = int(m_sum.get("migrate", -1))
    failed = int(m_sum.get("failed", 0))
    complete = int(r_sum.get("complete", -1))
    mismatch = int(r_sum.get("mismatch", 0))
    missing_payload = int(r_sum.get("missing-payload", 0))
    ok = (migrated == expected and failed == 0 and complete == expected
          and mismatch == 0 and missing_payload == 0)

    print(f"w4-oracle: expected={expected} migrated={migrated} failed={failed} "
          f"complete={complete} mismatch={mismatch} missing-payload={missing_payload} "
          f"-> {'ok' if ok else 'ASSERTION FAILED'}")
    if not a.keep_store:
        shutil.rmtree(store, ignore_errors=True)
        shutil.rmtree(scratch, ignore_errors=True)

    if not ok:
        return 30

    return 0


if __name__ == "__main__":
    sys.exit(main())
