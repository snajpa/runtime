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
(`warmup` | `measure`) — the harness sets it per block; `leg` is the harness's
per-position token (`A1..A2`/`B1..B2`) and `side` comes from `PERF_SIDE`
(`baseline` | `candidate`; fallback `candidate` for envelope mode).

`missing` is the tool's `action:"missing"` metadata — an artifact kind absent
in the fixture (e.g. the `large` class carries no memfile headers, so
header-c1 reports `missing=4`). It is by design, **not** a correctness signal;
the correctness check is the oracle's `missing-payload=0` (agent0 2226 /
lane F 2222).

`chunk` is the perf/2 object `{i,n}` (1-based) — `{1,1}` for block metrics,
`{k,chunks}` for the per-artifact latency arrays. Position-leg tokens resolve
to their side's tree and share the side-keyed tool cache; the comparator
normalizes `A*→A`/`B*→B` when pairing (identity keeps the raw leg). The earlier
duplicate-identity conflict
(`~/ai/logs/e2b-lane-b/perf-w4-abba-identity-finding.md`) is resolved by the
per-position tokens + `PERF_SIDE`.

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
  `PERF_TREE_DIR` (or `${PERF_TREES_DIR:-/root/ai/worktrees/e2b}/perf-<side>`)
  into the cache. First use builds atomically (tmp + `mv`) with the log at
  `$FIXTURES/.bin/build-<side>.log`; failures print its tail. The generator
  (`fixtures/genstore.sh`) rebuilds the same way, log `.cache/bin/build.log`
  under the perf log root.
- `PERF_W4_DRY=1` — plan only.
- `PERF_W4_ORACLE_CLASS` — oracle class override (default `tiny`).
- `PERF_STAGE`, `PERF_LEG`, `PERF_SIDE`, `PERF_ENV_CLASS`, `PERF_CAPTURE`
  (diagnostics).
