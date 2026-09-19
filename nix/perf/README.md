# nix/perf — R23 performance-regression suite

Orchestrator skeleton for the frozen R23 design (v1.0, reviewer1 v1-PASS
2026-09-19). The design record lives with the project notes
(`~/ai/docs/projects/e2b/subprojects/perf-regression/`); this directory is the
product-tree implementation.

## What it enforces (from the design)

- **Non-gating**: never runs in `make tests`; opt-in `make perf-regress` only.
  It does not gate the frozen `snajpa-integration` landing or the push path.
- **Fixed protocol**: 2 warmups + 12 randomized ABBA/BAAB blocks per
  cell × class; no post-result discard or retry; seeds recorded.
- **Fixed-band A/A** with the WARN-vs-FAIL **discriminability** gate
  (`false_fails == 0` is necessary, not sufficient; inability ⇒ INCONCLUSIVE).
- **Five outcomes / exits**: PASS 0 · FAIL 10 · WARN 20 · INCONCLUSIVE 30 ·
  SETUP_ERROR 40. Only PASS exits 0; environment/validity failures never grade.
- **Persistence**: T1 stream `perf-runs.jsonl` (perf/2 schema, lane F); exact
  command/env/seeds/CI inputs; recorded `replay_cmd`; raw samples; retained
  diagnostic `perf.data`. Artifacts under `~/ai/logs/e2b-perf/<run-id>/`
  (never tmpfs; 40 GB floor).

## Usage (skeleton)

    ./nix/perf/perf-suite.sh selftest
    ./nix/perf/perf-suite.sh run --candidate <dir|ref> [--baseline <dir|ref>] \
        --env quiet|busy [--workloads W1,W4] [--blocks 12] [--warmups 2] \
        [--seed N] [--dry-run]
    ./nix/perf/perf-suite.sh calibrate --candidate <dir|ref> --env quiet|busy
    ./nix/perf/perf-suite.sh compare --run <run-id>
    ./nix/perf/perf-suite.sh flamegraph --run <run-id> [--mode kernel|user|combined]
    ./nix/perf/perf-suite.sh report --run <run-id>

`--dry-run` prints the block/leg plan without touching a workload — use it to
verify wiring.

## Contracts (owners)

- `schema/emit.sh` — **lane F** (landed): the T1 writer.
  `emit.sh append --stream <path> --kind <K> --phase <p> --mode <m>` (body on
  stdin; `--run-id` overrides `PERF_RUN_ID`; `PERF_SUITE_REF`/`PERF_SUITE_SHA`
  fill `suite{}`) or `emit.sh append --run-dir <dir>` (stream =
  `<dir>/perf-runs.jsonl`); validates against the `perf-2` schema, stamps `ts`,
  appends write-then-fsync (never partial lines); exit 0 / 40. Modules emit
  through the harness, never write the stream directly.
- `report --replay|--verify --run-dir <dir> [--baseline <p>]` — **lane F**:
  reads only (`replay-verify` contract). The verdict side (`calibrate` fixed
  bands + discriminability gate; `compare`) is `schema/compare.sh` — **landed**
  (`76697ce80`, with `bands.json` v0 defaults; five outcomes, exact exits).
- `modules/<w>.sh` — workload modules; subcommands:
  - `list` → profile ids (one per line);
  - `oracle <profile>` → step-0 correctness gate (non-zero exit on failure);
  - `run <profile> <leg> <block> <leg-dir>` → perform the workload; raw
    samples to `<leg-dir>/samples.jsonl`; artifacts into `<leg-dir>`; no
    verdicts;
  - `profile <profile> <leg-dir>` → matched-diagnostics capture (perf/bpftrace,
    pprof) — never gating.
  - measurement goes through lane C's wrapper: `perf-capture.sh run
    clean|diagnostic <unit> <leg-dir> [--lock] -- <cmd>` (raw records →
    `samples.jsonl`; diagnostics retain `perf.data` + SVG).
  The harness passes `PERF_ENV_CLASS`, `PERF_LEG` (per-position token
  `A1..A2`/`B1..B2`; the comparator normalizes `A*→A`/`B*→B`), `PERF_SIDE`
  (`baseline|candidate`), `PERF_TREE_DIR`, `PERF_STAGE`, and
  `PERF_RUN_ID/PERF_PHASE/PERF_MODE/PERF_SUITE_*`.
  Owners: W1 lane D · W4 lane B · W6 lane A (W3/W5 assigned during
  implementation).
- `trees.sh` — **lane A**: materialize/validate the baseline+candidate trees
  (records sha/describe/dirty/go/tool-versions/
  flake + build hashes).
- `perf-capture.sh` / `perf-rusage.py` — **lane C** (landed): clean/trace/
  diagnostic capture wrapper + exact rusage summary; `flamegraph <unit> <dir> [mode]` regeneration (kernel/user via dso filtering); env knobs `FGRAPH` /
  `TRACE_EVENTS` / `PERF_FREQ` / `CALLGRAPH`.
- `busy-ref.sh` — **lane E**: busy-class reference load (`run` one-shot;
  `start`/`stop` session spanning the measured window). Emits the identity
  (`recipe`/`params_digest`) + signature/envelope JSON (`busy-load.json` in
  the run dir); the harness wires it into `--env busy` runs and records the
  `env.busy_load` fields; envelope miss ⇒ overall INCONCLUSIVE (exit 30).
- `guest-capabilities.sh` — **lane E**: guest-side capability + covariate
  probe; stdout = schema-kind records (`capability`/`env`) for the harness
  emitter. Run post-boot, outside timed phases (W6 per reviewer1 2137/2140);
  sealed `requirement` classes drive the status mapping.
- Device window — **lane C**: device-touching workloads run under the same
  `flock` contract (`E2B_NBD_TEST_LOCK` else `$TMPDIR/e2b-nbd-device-tests.lock`),
  wait recorded as a covariate; W4/W5 never hold it.
- Suite run lock — **lane E**: `run`/`calibrate` take `flock` on
  `$PERF_LOG_ROOT/.suite.lock` before the run dir is created; a second run
  refuses (exit 40) unless `PERF_SUITE_WAIT=<s>` is set for a bounded wait
  (a real wait records a `note` with `wait_s`). Any new module invocation
  added to the dispatch must carry `9>&-` (fd 9 must never reach modules,
  VMs or daemons — `0e896a341`; lane C 2385 watch item).
- Stale-holder containment — **lane E** (**retraction of the earlier
  move-aside recipe**, reviewer1 2373): a live child can still hold the lock's
  open file description after its suite exits — do **not** move/delete the
  lock path and continue (a moved path is a disjoint rendezvous / lock
  partition, and a fresh path is not proof the protected resource is idle).
  Identify the holder (`fuser -v $PERF_LOG_ROOT/.suite.lock`); its owner
  verifies the resource is idle and releases/stops it **within its own task
  scope**; reconcile the old inode. Formal/measurement-grade runs stay frozen
  until then. The fd-inheritance fix (`0e896a341`) prevents new instances only.

## Status

Dispatch v1 (2026-09-19): the run path is wired end-to-end — per cell
(oracle-first), 2 warmup + 12 measured ABBA/BAAB blocks with emitted `block`
records, module `run` legs, and each leg's `samples.jsonl` fed to the T1
emitter. Modules landed: W4 (lane B) · W6 (lane A); W1 pending (lane D).
`emit.sh` + `compare.sh` + `busy-ref.sh` + `guest-capabilities.sh` + the
capture wrapper are in. `selftest`, `--dry-run`, `calibrate`, `compare` and
`report --replay|--verify` work today.
