# S3 dev-env rehearsal: mixed-version storage on one object store

Stage S3 of the [big-change readiness plan](../../../docs/projects/e2b/subprojects/scale-change-readiness.md)
§7: the code-side half of "replicate the dev environment so it is most similar
to probable production workloads". Stage 1 (the dev VM, transports, guest tests)
and stage 2 (object-count-shaped data) are prerequisites; this harness adds the
*fleet* dimension — two versions of the runtime writing and reading the same
live storage — which is where the 200 PB constraints (NFR-9) actually bite.

## What it does

The same driver source is built twice, once against an **old checkout** and once
against a **new checkout** of the e2b runtime. The storage and header code is
the only difference between the binaries, so running them against one shared
object store *is* a mixed-version fleet. The matrix:

| step | writer | reader | expected outcome |
|------|--------|--------|------------------|
| probe ×2 | old, new | same version | ok — both can round-trip through the store |
| upgrade leg | **new** | **old** | ok — the rollback binary must read the rollout-created artifacts; a loud refusal fails the matrix (rollback below the reader floor is blocked, never silent); never a misread |
| rollback leg | **old** | **new** | ok — new code must read what the fleet already wrote |
| stranding check | — | both | every object the other version wrote still exists |

Outcomes are classified per phase: `ok`, `rejected` (loud, allowed), `misread`
(data came back wrong — the failure this exists to catch) and `error`. The
matrix aggregates the required outcomes and exits non-zero if any failed: a
driver that exited non-zero, a fault read that did not name the tampered
entry, or a rollback read the reader floor blocks. Every phase is still
recorded and rendered (`render.sh` runs either way).

Payload sizes must be a multiple of the block size (4096): header mappings are
block-aligned, so `header.NewHeader` refuses anything else with
`not block-aligned` — a custom payload that is not a multiple fails loudly at
write time.

## Status (first runs, 2026-09-19)

Against Silo in the dev VM, with the old node at upstream `44a8a7549` and the
new node at the delivery tip — **all phases pass**:

| step | result |
|------|--------|
| `ensure-bucket` | ok — the driver creates the bucket through the SDK against the same spec |
| `probe` (old, new) | ok — compressed (zstd, 2 MiB frames) round-trip verified in both versions |
| `write` (new, then old) | ok — 8 builds × 7 MiB, `StoreFile` checksums match, V5 headers with frame tables stored |
| `read` (old reading new, new reading old) | ok — 8/8 artifacts each way, header versions `v5×8` |
| `exists` (both directions) | ok — 16/16 objects per side; nothing stranded across the upgrade and rollback legs |

Reads go through the cache layer (`storage.WrapInNFSCache`) in
`MemoryChunkSize`-aligned ranges, one range reader per range so every frame's
CRC is verified on `Close` — the same path production uses
(`packages/orchestrator/pkg/sandbox/block/streaming_chunk.go`). Reading through
the *raw* provider reader instead returns a single frame, which is what made the
first runs red; that is a harness property, not a storage-layer bug.

## Running it

```sh
# Silo must be up in the dev VM (see "Object store" below)
./run-matrix.sh                       # upstream 44a8a7549 → delivery tip
E2B_PROFILE=big ./run-matrix.sh       # sized for a big box
E2B_PROFILE=tiny ./run-matrix.sh      # quick smoke on a small one
```

The profiles ("tiny"/"small"/"big", or "auto" which scales builds and payload
size with the cores and memory of the box it runs on) are what makes the same
harness useful on a laptop-sized dev machine and on a production-shaped node.

## Object store (Silo)

Per the operator: Silo (`pgsty/silo`, the maintained MinIO fork) is the local
object store, matching what the repo's own tests use (S-53).

```sh
docker run -d --name silo -p 9000:9000 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  -v /srv/silo-data:/data pgsty/silo:latest server /data
```

The default storage URL is
`s3://e2b-rehearsal?endpoint=http://127.0.0.1:9000&s3ForcePathStyle=true&region=us-east-1`
with `minioadmin:minioadmin` credentials; override with `E2B_STORAGE_URL`.

## Files

- `driver/main.go` — the phases (`probe`, `write`, `read`, `exists`, `prune`),
  using the runtime's own storage and header APIs (`StoreFile`, `StoreHeader`,
  `LoadHeader`) so what is rehearsed is the production code path.
- `build.sh` — builds the driver against `E2B_CHECKOUT`, stamping the checkout
  and revision into the binary (`s3-rehearsal versions` prints it).
- `run-matrix.sh` — builds both versions, ships them into the dev VM, runs the
  matrix there, renders the results.
- `render.sh` — JSONL → Markdown.

## What this does *not* cover yet

- multiple orchestrator processes/node-fan-out and cross-node peer prefetch
  (the next step, S3 proper at the process level);
- the NFS chunk cache (needs an NFS server in the VM);
- GC/lifecycle rule changes under rehearsal (`prune` is the primitive);
- real 10¹⁰-object counts — see the note's §7 "not replicable locally".

## Legs and switches

The base matrix always runs: probe → new writes/old reads → old writes/new
reads → existence checks on both sides. These switches add legs, sized by the
same profiles (`tiny`/`small`/`big`/`auto`):

| switch | leg |
|--------|-----|
| `E2B_REHEARSAL_NODES=n` | `n` concurrent nodes *mixing versions*: even nodes write with the new binary, odd with the old, then each node reads its neighbour's artifacts with the opposite version, plus existence checks |
| `E2B_REHEARSAL_SPRAY=n` | object-count shape: `n` small objects (4 KiB) written by `E2B_REHEARSAL_SPRAY_CONCURRENCY` workers (default cores/2, 4..32), with throughput and p50/p95/p99; then the inventory (`count`), a **dry run** of the destructive step (`purge --dry-run`), the real `purge` (the GC/delete cost) and a second inventory proving the prefix is empty |
| `E2B_REHEARSAL_SPRAY_RETRIES=n` | attempts per object write in the spray (default 3, capped backoff); the leg reports `N retries (x% of writes)` so saturation stays visible |
| `E2B_REHEARSAL_FLAG_ROLLBACK=1` | format-affecting setting rehearsed end to end: the old build writes the older header format (in the **product layout**, so the runtime's own tooling can read it), the new build reads it, the new build writes the current format, the old build reads that, then the **runtime's `migrate-builds`** backfills the older artifacts and a `reconcile` pass confirms every reference resolves — after which both readers must still read them and nothing may be stranded. Checkouts without the tool fall back to the harness `migrate` phase and say so |
| `E2B_REHEARSAL_PEER=1` | peer prefetch with two node processes: an old-build peer serves over the repository's chunk service, a new-build client fetches ranges (and the other way around), every range is byte-compared against the store and hashed, and both latencies are reported |
| `E2B_REHEARSAL_FAULT=1` | fault injection: overwrite one artifact with different bytes, then read it cold with the other version; the verdict is `ok` only if the tampered entry itself is *named* in a loud refusal or a misread — an unrelated refusal is not detection |
| `E2B_REHEARSAL_NFS=1` | put every node's chunk cache on the VM's NFS export (`/mnt/nfs-cache`) instead of a local temp dir |
| `E2B_REHEARSAL_SOAK=k` | repeat the whole matrix `k` times (drift/soak) |

## Evidence (2026-09-19, dev VM, Silo)

- sequential matrix: green — 8/8 artifacts each way, v5 headers, 16/16 objects
  present on both sides
- fan-out: 3 concurrent nodes, versions mixed, every write ok and every
  cross-version read 8/8
- object-count ramps (32 writers, store-only writes):
  - **100k** — 100,000 × 4 KiB written in 5m46s (**289 objects/s**, p50 89 ms,
    p95 191 ms, p99 649 ms); inventory counted exactly 100,000 objects /
    390.6 MiB in **16.0 s**; prefix purge **1m16.6 s**; count afterwards 0 in
    17 ms
  - **725k (first 1M attempt, 64 writers)** — the spray stopped at 725,090
    objects with `PutObject ... context deadline exceeded`: the repository's own
    write budget (`awsWriteTimeout`, 30 s,
    `packages/shared/pkg/storage/storage_aws.go`) firing. The ramp now retries
    writes (bounded, reported as "N retries (x% of writes)"). The inventory of
    those 725,090 objects took **145.2 s** and the purge **12m31.7 s**
    (≈1.04 ms/object, about 5× the per-object listing cost)
  - **1M (re-run with 32 writers, completed)** — 1,000,000 × 4 KiB written in
    50m59s (**327 objects/s, zero retries**, p50 84.9 ms, p95 175.1 ms,
    p99 290.4 ms); the inventory counted exactly 1,000,000 objects / 3.8 GiB in
    **119.2 s**; the dry run reported 1,000,000 objects / 3.8 GiB in **93.6 s**;
    the real purge took **16m28.5 s** (≈0.99 ms/object); the count afterwards
    read 0 in 14 ms. **The 725k wall was concurrency-induced, not
    object-count-induced**: the same 1M workload at 32 writers needed no
    retries at all, while 64 writers saturated the single node past the 30 s
    write budget — what this store cannot take is concurrent writer pressure,
    and the retry reporting is what makes that visible.
- small-shape object-count (1500 × 4 KiB): p50 18 ms / p95 34 ms / p99 50 ms;
  inventory exactly 1500 (5.9 MiB); prefix delete 1.7 s; count afterwards 0
- flag rollback (`E2B_REHEARSAL_FLAG_ROLLBACK=1`): the old build wrote v4
  headers → the new build read them (`v4×2`); the new build wrote v5 → the old
  build read them (`v5×2`); `migrate` rewrote the v4 artifacts into v5
  (8.0 MiB in 1.05 s); **both** builds then read the backfilled artifacts
  (`v5×2`) and existence was 4/4 on both sides — nothing stranded
- peer prefetch (`E2B_REHEARSAL_PEER=1`): old-build peer → new-build client,
  8.0 MiB, peer p50 182.8 ms vs the same ranges read from the store 73.7 ms;
  new-build peer → old-build client, 8.0 MiB, peer p50 238.2 ms vs 78.2 ms.
  Every range was byte-compared against the store and the whole file hashed in
  both directions. The peer is *slower* here on purpose: this harness's peer
  reads through to the same store instead of serving from a warm template
  cache, so the leg proves the protocol, the mixed-version interop and the
  verification — not the cache benefit production gets
- flag rollback through the **runtime's own tool** (`nix/rehearsal` writes the
  product layout, the tool does the work): old build wrote 4 artifacts with v4
  headers → new build read them (`v4×4`); new build wrote v5 → old build read
  them (`v5×4`); `migrate-builds -apply` reported **migrated=4 skipped=0
  missing=0**; a `reconcile -verify` pass reported **complete=4
  missing-payload=0 mismatch=0**; both builds then read all four back
  (`v5×4`) with **8/8 objects present on both sides** — nothing stranded
- fault injection: the tampered artifact was refused loudly ("magic number
  mismatch") in both soak rounds — never silently accepted
- NFS: caches land on `/mnt/nfs-cache/<run>/<node>` (105 files, 85 MiB in the
  first run)
- soak: 2 rounds, 28 `ok` outcomes, no errors or misreads
