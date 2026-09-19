#!/usr/bin/env python3
"""perf/2 comparator (lane F): calibrate | compare | replay | verify.

Fixed bands only (schema/bands.json - never fitted); one-sided paired-bootstrap
bounds (5th/95th percentiles of the severity), Holm across the metric family;
five outcomes with distinct exits:
  PASS 0 / FAIL 10 / WARN 20 / INCONCLUSIVE 30 / SETUP_ERROR 40 .
Gate rule (design §5): metrics with "gate": false are WARN-only (a would-be
FAIL is capped at WARN until calibration review enables the gate). Warmup
blocks (stage="warmup", or block records with warmup=true) are retained in
T1 but excluded from every statistic here."""
import json
import os
import random
import sys

EXITS = {"pass": 0, "fail": 10, "warn": 20, "inconclusive": 30,
         "setup_error": 40}


def fail(msg):
    print("compare: setup_error: " + msg, file=sys.stderr)
    sys.exit(40)


def pct(xs, p):
    s = sorted(xs)
    if not s:
        return float("nan")
    k = min(len(s) - 1, max(0, int(round((len(s) - 1) * p))))
    return s[k]


def median(xs):
    s = sorted(xs)
    n = len(s)
    if n == 0:
        return float("nan")
    return s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2.0


def load_stream(path):
    recs = []
    try:
        with open(path, "r", encoding="utf-8") as fh:
            for n, line in enumerate(fh, 1):
                line = line.strip()
                if not line:
                    continue
                try:
                    recs.append(json.loads(line))
                except Exception as exc:
                    fail("invalid JSON at line %d: %s" % (n, exc))
    except OSError as exc:
        fail("cannot read stream %s: %s" % (path, exc))
    return recs


def group_samples(recs):
    """Group sample records per (workload, profile, metric) and side.

    Warmup blocks are retained in T1 but excluded from every statistic here:
    a sample is skipped when its stage is "warmup", or when its block index is
    flagged warmup=true by a block record."""
    warmup_blocks = set()
    for r in recs:
        if r.get("record") == "block" and r.get("warmup"):
            try:
                warmup_blocks.add(int(r.get("index")))
            except (TypeError, ValueError):
                pass
    g = {}
    for r in recs:
        if r.get("record") != "sample":
            continue
        if r.get("stage") == "warmup":
            continue
        try:
            blk = int(r.get("block") or 0)
        except (TypeError, ValueError):
            continue
        if warmup_blocks and blk in warmup_blocks:
            continue
        m = r.get("metric") or {}
        key = (r.get("workload"), r.get("profile"), m.get("id"), m.get("unit"),
               r.get("unit_id"))
        leg = r.get("leg") or ""
        side = "A" if leg.startswith("A") else "B" if leg.startswith("B") else leg
        vals = [float(x) for x in (r.get("samples") or [])]
        if not vals:
            continue
        g.setdefault(key, {}).setdefault(side, {}).setdefault(blk, []).extend(vals)
    return g


def band_for(bands, mid):
    m = (bands.get("metrics") or {}).get(mid)
    if m is None:
        m = dict(bands.get("defaults") or {})
        m["defaulted"] = True
    return m


def paired(per_side):
    a, b = per_side.get("A", {}), per_side.get("B", {})
    es = []
    for blk in sorted(set(a) & set(b)):
        ma, mb = median(a[blk]), median(b[blk])
        if ma:
            es.append((mb - ma) / ma)
    return es


def classify(es, band, iters, seed):
    c = -1.0 if band.get("direction") == "higher_better" else 1.0
    s = [c * e for e in es]
    rnd = random.Random(seed)
    n = len(s)
    bs = []
    if n:
        for _ in range(iters):
            t = 0.0
            for _ in range(n):
                t += s[rnd.randrange(n)]
            bs.append(t / n)
    s_lo, s_hi = (pct(bs, 0.05), pct(bs, 0.95)) if bs else (float("nan"),) * 2
    s_pt = median(s)
    p = ((sum(1 for x in bs if x <= 0.0) + 1.0) / (iters + 1.0)) if bs else 1.0
    bw, bf = abs(band.get("warn", 0.05)), abs(band.get("fail", 0.10))
    verdict, reason = "inconclusive", "interval cannot rule WARN out"
    if es and s_lo >= bf and p <= 0.05:
        verdict, reason = "fail", "severity CI fully beyond the FAIL band (Holm-supporting)"
        if not band.get("gate", False):
            verdict, reason = "warn", "FAIL capped at WARN (gate disabled until calibration review)"
    elif es and s_pt >= bw:
        verdict, reason = "warn", "effect beyond the WARN band"
    elif es and s_hi < bw:
        verdict, reason = "pass", "interval rules WARN out"
    return {"direction": band.get("direction"), "warn": bw, "fail": bf,
            "gate": bool(band.get("gate", False)), "n_blocks": n,
            "severity_pt": None if not es else round(s_pt, 6),
            "severity_lo": None if not bs else round(s_lo, 6),
            "severity_hi": None if not bs else round(s_hi, 6),
            "p_one_sided": round(p, 6), "verdict": verdict, "reason": reason}


def holm_adjust(rows):
    pv = [r["metrics"][k]["p_one_sided"] for r, k in rows]
    m = len(pv)
    order = sorted(range(m), key=lambda i: pv[i])
    run, adj = 0.0, [0.0] * m
    for rank, i in enumerate(order):
        v = min(1.0, pv[i] * (m - rank))
        run = max(run, v)
        adj[i] = run
    for (r, k), a in zip(rows, adj):
        r["metrics"][k]["p_holm"] = round(a, 6)


def identity(r):
    m = r.get("metric") or {}
    ch = r.get("chunk") or {}
    return (r.get("run_id"), r.get("workload"), r.get("profile"), r.get("side"),
            r.get("leg"), r.get("block"), r.get("stage"), m.get("id"),
            m.get("unit"), r.get("unit_id"), ch.get("i"), ch.get("n"))


def duplicates(recs):
    """Count extra sample records sharing one identity (reviewer1 2175)."""
    seen = {}
    extra = 0
    example = None
    for r in recs:
        if r.get("record") != "sample":
            continue
        k = identity(r)
        seen[k] = seen.get(k, 0) + 1
        if seen[k] > 1:
            extra += 1
            if example is None:
                example = k
    return extra, example


def runtime_evidence_problem(recs):
    """Section 11 (reviewer1 2267/2275): a W7/W8 stream cannot yield a runtime
    resolution without the mandatory evidence. Returns (None|severity, reason)."""
    wl_samples = [r for r in recs if r.get("record") == "sample"
                  and r.get("workload") in ("W7", "W8")]
    if not wl_samples:
        return None, None

    def leg_side(r):
        leg = r.get("leg") or ""
        if leg.startswith("A"):
            return "baseline"
        if leg.startswith("B"):
            return "candidate"
        return None

    def record_side(r):
        return r.get("side") if r.get("side") in ("baseline", "candidate") else None

    for r in wl_samples:
        ls, es = leg_side(r), record_side(r)
        if ls and es and ls != es:
            return "inconclusive", ("explicit side %r disagrees with leg %r"
                                    % (r.get("side"), r.get("leg")))
    snaps = {}
    for r in recs:
        if r.get("record") == "snapshot" and r.get("snapshot_id"):
            snaps[r["snapshot_id"]] = r
    if not snaps:
        return "inconclusive", "missing snapshot identity record"
    used = {}
    snap_required = ("content_sha256", "source_tree", "firecracker", "kernel",
                     "rootfs", "init", "config", "vcpu", "memory_mib",
                     "cpu_template", "network", "ports", "uffd",
                     "object_store_state", "restore_state_dir",
                     "quiesced_marker", "class", "cache_state_evidence",
                     "reset_procedure")
    for sid, s in snaps.items():
        for key in snap_required:
            if key not in s:
                return "inconclusive", "snapshot %s lacks %s" % (sid, key)
    for r in wl_samples:
        s = snaps.get(r.get("snapshot_id"))
        if s is None:
            return "inconclusive", "sample links unknown snapshot_id %r" % (r.get("snapshot_id"),)
        if r.get("class") != s.get("class"):
            return "inconclusive", "sample class %r != snapshot class %r" % (
                r.get("class"), s.get("class"))
        used[(r.get("workload"), leg_side(r) or record_side(r),
               r.get("snapshot_id"), r.get("class"))] = True
    # cross-side eligibility (reviewer1 2300/2305): both sides must be format/
    # config/class/digest compatible. Source-tree hashes and snapshot ids are
    # recorded per side but never equated (the trees are the variable under test).
    fp_keys = ("firecracker", "kernel", "rootfs", "init", "config", "vcpu",
               "memory_mib", "cpu_template", "network", "ports", "uffd",
               "object_store_state", "class")
    ref_sid, ref_fp = None, None
    for sid in sorted({t_[2] for t_ in used}):
        s = snaps[sid]
        fp = {k: s.get(k) for k in fp_keys}
        if ref_fp is None:
            ref_sid, ref_fp = sid, fp
        elif fp != ref_fp:
            for k in fp_keys:
                if fp.get(k) != ref_fp.get(k):
                    return "inconclusive", ("cross-side fixture/config mismatch on %s"
                                            " (snapshots %s vs %s)" % (k, ref_sid, sid))
    pds = {r.get("profile_digest") for r in wl_samples}
    if len(pds) > 1:
        return "inconclusive", "profile_digest differs across sides/samples"
    wds = {r.get("workload_digest") for r in wl_samples if r.get("workload") == "W8"}
    if len(wds) > 1:
        return "inconclusive", "workload_digest differs across sides/samples"
    sides = {t_[1] for t_ in used}
    caps = [r for r in recs if r.get("record") == "capability"]
    for r in caps:
        if r.get("status") == "available" or r.get("substitute"):
            continue
        if r.get("requirement") == "to_execute":
            return "setup_error", "required-to-execute capability %r is %s" % (
                r.get("name"), r.get("status"))
        if r.get("requirement") == "validity":
            return "inconclusive", "validity capability %r is %s" % (
                r.get("name"), r.get("status"))
    scopes = {r.get("scope") for r in caps}
    if "host" not in scopes:
        return "inconclusive", "missing host capability comparability"
    for side in sorted(sides):
        if not any(r.get("scope") == "guest" and record_side(r) == side for r in caps):
            return "inconclusive", "missing guest capability comparability for side %s" % side
    fps = {r.get("host", {}).get("fingerprint") for r in recs if r.get("record") == "env"}
    fps.discard(None)
    if len(fps) > 1:
        return "inconclusive", "host fingerprint differs across env records; host capabilities cannot be shared"
    pd = next(iter(pds)) if len(pds) == 1 else None
    wd = next(iter(wds)) if len(wds) == 1 else None

    def oracle_matches(r, wl, side, sid, cls, role):
        if r.get("record") != "oracle" or r.get("passed") is not True:
            return False
        if r.get("workload") != wl:
            return False
        kind = r.get("kind") or ""
        if not (kind == wl or kind.startswith(wl + "/") or kind.startswith(wl + "-")):
            return False
        if record_side(r) != side or r.get("snapshot_id") != sid or r.get("class") != cls:
            return False
        if r.get("role") != role:
            return False
        if pd is not None and r.get("profile_digest") != pd:
            return False
        if wl == "W8" and wd is not None and r.get("workload_digest") != wd:
            return False
        return True

    for tri in sorted(used, key=lambda t_: (str(t_[0]), str(t_[1]), str(t_[2]), str(t_[3]))):
        wl, side, sid, cls = tri
        roles = ("ready",) if wl == "W7" else ("pre_identity", "post_integrity")
        for role in roles:
            if not any(oracle_matches(r, wl, side, sid, cls, role) for r in recs):
                return "inconclusive", ("missing passed %s %s-side oracle (snapshot %s, class %s)"
                                        % (role, side, sid, cls))
    return None, None


def summary(recs, bands, iters, seed, stream):
    env_invalid = any(r.get("record") == "env" and r.get("invalid_reason") for r in recs)
    dup_records, dup_example = duplicates(recs)
    ev, ev_reason = runtime_evidence_problem(recs)
    g = group_samples(recs)
    out = {"schema": "perf/summary/1", "stream": stream, "seed": seed,
           "bootstrap": {"iterations": iters, "method": "one-sided",
                          "ci": "5th/95th percentile"},
           "metrics": {}, "verdict": "inconclusive", "exit": 30}
    rows = []
    items = [] if ev else sorted(g, key=lambda k: (str(k[0]), str(k[1]), str(k[2]), str(k[4])))
    for (w, p, mid, unit, uid) in items:
        if mid is None:
            continue
        es = paired(g[(w, p, mid, unit, uid)])
        if not es:
            continue
        key = "%s|%s|%s|%s" % (w, p, mid, uid)
        row = classify(es, band_for(bands, mid), iters, seed)
        row.update({"workload": w, "profile": p, "metric": mid, "unit": unit,
                    "unit_id": uid})
        out["metrics"][key] = row
        rows.append((out, key))
    holm_adjust(rows)
    order = {"fail": 3, "inconclusive": 2, "warn": 1, "pass": 0}
    best = "pass" if out["metrics"] else "inconclusive"
    if out["metrics"]:
        best = max(out["metrics"].values(), key=lambda r: order[r["verdict"]])["verdict"]
    if env_invalid:
        best = "inconclusive"
        out["env_invalid"] = True
    if dup_records:
        best = "inconclusive"
        out["duplicate_identities"] = dup_records
        out["duplicate_example"] = list(dup_example) if dup_example else None
        print("compare: duplicate sample identities detected (%d extra records)"
              " - verdict INCONCLUSIVE (no regression outcome valid)" % dup_records,
              file=sys.stderr)
    out["verdict"] = best
    out["exit"] = EXITS[best]
    return out


def report_md(s):
    lines = ["# perf summary", "",
             "- verdict: **%s** (exit %d)" % (s["verdict"].upper(), s["exit"]),
             "- stream: `%s` · seed %s · bootstrap %d (%s)" % (
                 s["stream"], s["seed"], s["bootstrap"]["iterations"],
                 s["bootstrap"]["ci"]), ""]
    if s.get("env_invalid"):
        lines.append("- **invalid comparability** - overall INCONCLUSIVE (ruling 2)")
        lines.append("")
    lines += ["| cell | metric | n | sev (pt [lo, hi]) | bands (warn/fail) | p (holm) | verdict |",
              "|---|---|---|---|---|---|---|"]
    for k, r in sorted(s["metrics"].items()):
        lines.append("| %s | %s | %d | %s [%s, %s] | %.3f/%.3f | %.3f (%.3f) | **%s** |" % (
            k.split("|")[0] + "/" + str(r.get("profile")), r["metric"], r["n_blocks"],
            r["severity_pt"], r["severity_lo"], r["severity_hi"], r["warn"], r["fail"],
            r["p_one_sided"], r.get("p_holm", r["p_one_sided"]), r["verdict"]))
    return "\n".join(lines) + "\n"


def parse_common(argv):
    a = {"stream": None, "run_dir": None, "bands": os.path.join(os.path.dirname(os.path.abspath(__file__)), "bands.json"),
         "iters": 10000, "seed": int(os.environ.get("PERF_SEED", "1")), "out": None, "emit": False, "args": []}
    i = 0
    while i < len(argv):
        x = argv[i]
        if x == "--emit":
            a["emit"] = True
            i += 1
            continue
        if x in ("--stream", "--run-dir", "--run", "--bands", "--iters", "--seed", "--out"):
            i += 1
            if i >= len(argv):
                fail("%s needs a value" % x)
            v = argv[i]
            if x == "--stream":
                a["stream"] = v
            elif x in ("--run-dir", "--run"):
                a["run_dir"] = v
            elif x == "--bands":
                a["bands"] = v
            elif x == "--iters":
                a["iters"] = int(v)
            elif x == "--seed":
                a["seed"] = int(v)
            else:
                a["out"] = v
        else:
            a["args"].append(x)
        i += 1
    if a["run_dir"] and not os.path.isdir(a["run_dir"]):
        root = os.environ.get("PERF_LOG_ROOT", os.path.expanduser("~/ai/logs/e2b-perf"))
        cand = os.path.join(root, a["run_dir"])
        if os.path.isdir(cand):
            a["run_dir"] = cand
        else:
            fail("run dir not found: %s (also tried %s)" % (a["run_dir"], cand))
    if a["run_dir"]:
        a["stream"] = os.path.join(a["run_dir"], "perf-runs.jsonl")
    if not a["stream"]:
        fail("--stream <path> or --run-dir <dir> is required")
    return a


def calibrate(recs, bands, iters, seed, stream):
    dup_records, dup_example = duplicates(recs)
    if dup_records:
        print("compare: duplicate sample identities detected (%d extra records)"
              " - calibration INVALID" % dup_records, file=sys.stderr)
        return {"schema": "perf/resolution/1", "stream": stream, "valid": False,
                "duplicate_identities": dup_records,
                "duplicate_example": list(dup_example) if dup_example else None,
                "rows": []}
    g = group_samples(recs)
    rows = []
    items = [] if ev else sorted(g, key=lambda k: (str(k[0]), str(k[1]), str(k[2]), str(k[4])))
    for (w, p, mid, unit, uid) in items:
        if mid is None:
            continue
        es = paired(g[(w, p, mid, unit, uid)])
        if not es:
            continue
        band = band_for(bands, mid)
        c = -1.0 if band.get("direction") == "higher_better" else 1.0
        sv = [abs(c * e) for e in es]
        bw, bf = abs(band.get("warn", 0.05)), abs(band.get("fail", 0.10))
        noise95 = pct(sv, 0.95)
        gap = bf - bw
        able = noise95 < gap
        ff = sum(1 for x in sv if x >= bf)
        rows.append({"workload": w, "profile": p, "metric": mid, "unit": unit,
                     "unit_id": uid,
                     "sample_count": len(es), "bands": {"warn": bw, "fail": bf},
                     "method": "fixed", "fitted": False,
                     "discriminability": {"able_to_separate_warn_fail": bool(able),
                                           "n": len(es), "ci_level": 0.95,
                                           "rule": "null p95 |severity| < (FAIL - WARN)"},
                     "aa_false_fails": ff, "noise_p95": round(noise95, 6)})
    valid = bool(rows) and all(r["discriminability"]["able_to_separate_warn_fail"]
                               and r["aa_false_fails"] == 0 for r in rows)
    return {"schema": "perf/resolution/1", "stream": stream, "valid": valid, "rows": rows}


def emit_resolutions(stream_path, run_id, rows):
    import subprocess
    emit = os.path.join(os.path.dirname(os.path.abspath(__file__)), "emit.sh")
    for r in rows:
        rec = {"record": "resolution", "metric": {"id": r["metric"], "unit": r["unit"]},
               "unit_id": r.get("unit_id"),
               "sample_count": r["sample_count"], "bands": r["bands"],
               "method": r["method"], "fitted": r["fitted"],
               "discriminability": r["discriminability"],
               "aa_false_fails": r["aa_false_fails"]}
        p = subprocess.run(["sh", emit, "append", "--stream", stream_path,
                            "--kind", "resolution", "--phase", "calibrate",
                            "--mode", "clean", "--run-id", run_id],
                           input=json.dumps(rec), text=True)
        if p.returncode != 0:
            fail("resolution emit failed for %s (rc=%d)" % (r["metric"], p.returncode))


def main(argv):
    if not argv or argv[0] in ("-h", "--help"):
        print(__doc__)
        return 0
    cmd, argv = argv[0], argv[1:]
    a = parse_common(argv)
    try:
        with open(a["bands"], "r", encoding="utf-8") as fh:
            bands = json.load(fh)
    except Exception as exc:
        fail("cannot read bands %s: %s" % (a["bands"], exc))
    recs = load_stream(a["stream"])
    s = summary(recs, bands, a["iters"], a["seed"], a["stream"])
    if cmd == "calibrate":
        res = calibrate(recs, bands, a["iters"], a["seed"], a["stream"])
        dest = a["out"] or "-"
        text = json.dumps(res, indent=1, sort_keys=True) + "\n"
        if dest == "-":
            sys.stdout.write(text)
        else:
            open(dest, "w", encoding="utf-8").write(text)
            print("calibration written: %s (valid=%s)" % (dest, res["valid"]))
        if a["emit"]:
            rid = next((r.get("run_id") for r in recs if r.get("run_id")), None)
            if not rid:
                fail("cannot determine run id from the stream for --emit")
            emit_resolutions(a["stream"], rid, res["rows"])
            print("resolution records emitted: %d" % len(res["rows"]))
        return 0 if res["valid"] else 30
    if cmd == "compare":
        dest = a["out"] or os.path.join(os.path.dirname(os.path.abspath(a["stream"])), "perf-summary.json")
        open(dest, "w", encoding="utf-8").write(json.dumps(s, indent=1, sort_keys=True) + "\n")
        md = os.path.splitext(dest)[0] + ".md"
        open(md, "w", encoding="utf-8").write(report_md(s))
        print("compare: %s (exit %d) -> %s" % (s["verdict"].upper(), s["exit"], dest))
        return s["exit"]
    if cmd == "replay":
        text = json.dumps(s, indent=1, sort_keys=True) + "\n"
        if a["out"] and a["out"] != "-":
            open(a["out"], "w", encoding="utf-8").write(text)
            print("replay written: %s" % a["out"])
        else:
            sys.stdout.write(text)
        return 0
    if cmd == "verify":
        stored = None
        if a["run_dir"]:
            p = os.path.join(a["run_dir"], "perf-summary.json")
            if os.path.exists(p):
                stored = json.load(open(p, "r", encoding="utf-8"))
        if stored is None:
            fail("verify: stored perf-summary.json not found (pass --run-dir)")
        norm = lambda d: json.dumps({k: v for k, v in d.items() if k != "stream"}, sort_keys=True)
        if norm(stored) == norm(s):
            print("verify: OK")
            return 0
        print("verify: DRIFT - rebuilt summary differs from the stored render", file=sys.stderr)
        return 40
    fail("unknown subcommand %r (calibrate|compare|replay|verify)" % cmd)
    return 40


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
