#!/usr/bin/env python3
"""W4 block statistics: read `w4-raw.jsonl` + per-block reports and print
block-level median/MAD/spread for wall/CPU/RSS/scratch plus per-artifact
latency percentiles.

usage: w4-stats.py <run-dir>
"""
import json
import pathlib
import statistics
import sys


def pct(xs, p):
    xs = sorted(xs)
    if not xs:
        return 0.0
    k = (len(xs) - 1) * p
    f = int(k)
    c = min(f + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)


def stats(vals):
    vals = sorted(vals)
    med = statistics.median(vals)
    mad = statistics.median([abs(v - med) for v in vals]) if len(vals) > 1 else 0.0
    spread = (vals[-1] - vals[0]) / med * 100 if med else 0.0
    return (f"n={len(vals)} med={med:.2f} min={vals[0]:.2f} max={vals[-1]:.2f} "
            f"mad={mad:.2f} spread={spread:.1f}%")


def main():
    d = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
    raw = d / "w4-raw.jsonl"
    if not raw.exists():
        sys.exit(f"no {raw}")
    recs = [json.loads(line) for line in raw.read_text().splitlines() if line.strip()]
    blocks = [r for r in recs if r["label"].startswith("block") and r["rc"] == 0]
    print(f"run_id(s): {sorted({r['run_id'] for r in recs})}  blocks(rc=0)={len(blocks)}/"
          f"{len([r for r in recs if r['label'].startswith('block')])}")
    for metric in ("wall_s", "user_s", "sys_s", "peak_rss_kib", "scratch_peak_bytes"):
        vals = [r[metric] for r in blocks]
        if vals:
            print(f"{metric}: {stats(vals)}")

    for label in sorted({r["label"] for r in blocks}):
        p = d / f"report-{label}.json"
        if not p.exists():
            continue
        ds = []
        for line in p.read_text().splitlines():
            if not line.strip():
                continue
            r = json.loads(line)
            if r.get("action") == "migrate":
                ds.append(r["duration_ms"])
        if ds:
            print(f"{label}: migrate_n={len(ds)} p50={pct(ds, 0.5):.0f} "
                  f"p95={pct(ds, 0.95):.0f} p99={pct(ds, 0.99):.0f} max={max(ds)} ms")


if __name__ == "__main__":
    main()
