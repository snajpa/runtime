# W4 module — migration benches (lane B)

Contract: `nix/perf/modules/w4.sh` (`list | oracle <profile> [outdir] |
run <profile> <leg> <block> <leg-dir> | profile <profile> <leg-dir>`); the
harness drives the fixed 2-warmup + 12-block structure and grades. The module
produces oracle evidence + raw samples only — no verdicts.

## Profiles

| profile | store class | tool mode | params |
|---|---|---|---|
| header-c1 | large | migrate | header-only, C1 |
| zstd-c1 | large | migrate | raw→zstd, C1 |
| zstd-c4 | large | migrate | raw→zstd, C4 |
| latency-zstd-c1 | latency | migrate | raw→zstd, C1 (tails) |
| reconcile-c1 | latency | reconcile `-verify` | C1 (both kinds present -> complete) |

## Records

`run` appends per-metric `perf/2` **sample** records to
`<leg-dir>/samples.jsonl` (emitter-validated): `wall_s`, `user_cpu_s`,
`sys_cpu_s` (s); `peak_rss_mib`, `scratch_peak_mib` (MiB);
`migrate_latency_ms` (ms, per-artifact chunks of ≤1024);
`migrated`/`skipped`/`missing`/`failed` (count). `unit_id` is `block` for
block-level metrics and `artifact` for per-artifact ones; the envelope is
stamped by `schema/emit.sh`. The rich block summary is kept as
`<leg-dir>/block.json` (artifact). `stage` comes from `PERF_STAGE`
(`warmup` | `measure`) — the harness should set it per block; `leg` is passed
through (`A` | `B` | `AA`), `side` is `candidate` (W4 is envelope mode).

`chunk` is the perf/2 object `{i,n}` (1-based) — `{1,1}` for block metrics,
`{k,chunks}` for the per-artifact latency arrays. Under ABBA/BAAB the harness
runs each leg twice per block and both runs forward (offset-aware `feed_leg`),
so one block yields two sample records per (leg, block, metric) — the replicate
pair `compare.paired()` medians. Open question (duplicate-identity guard vs
replicates): `~/ai/logs/e2b-lane-b/perf-w4-abba-identity-finding.md`.

## Oracle

`oracle <profile>` runs the correctness leg on the **tiny** class by default
(`PERF_W4_ORACLE_CLASS` overrides): migrate a store copy, assert counters
(`migrated == ids × kinds`, `failed == 0`), then `reconcile -verify`
(`complete == expected`, `mismatch == 0`, `missing-payload == 0`). Exit 0 pass
/ 30 assertion-failed (no perf verdict from an unverified cell) / 40 setup
error.

## Environment

- `PERF_W4_FIXTURES` — fixture cache (default `~/ai/logs/e2b-perf/fixtures`).
- `PERF_W4_TOOL` — tool binary override; otherwise built from
  `PERF_TREE_DIR` (or `${PERF_TREES_DIR:-/root/ai/worktrees/e2b}/perf-<leg>`)
  into the cache. First use builds atomically (tmp + `mv`) with the log at
  `$FIXTURES/.bin/build-<leg>.log`; failures print its tail. The generator
  (`fixtures/genstore.sh`) rebuilds the same way, log `.cache/bin/build.log`
  under the perf log root.
- `PERF_W4_DRY=1` — plan only.
- `PERF_W4_ORACLE_CLASS` — oracle class override (default `tiny`).
- `PERF_STAGE`, `PERF_LEG`, `PERF_ENV_CLASS`, `PERF_CAPTURE` (diagnostics).
