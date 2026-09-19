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
| upgrade leg | **new** | **old** | ok, or a loud rejection (old refusing what it cannot parse); never a misread |
| rollback leg | **old** | **new** | ok — new code must read what the fleet already wrote |
| stranding check | — | both | every object the other version wrote still exists |

Outcomes are classified per phase: `ok`, `rejected` (loud, allowed), `misread`
(data came back wrong — the failure this exists to catch) and `error`. `misread`
exits non-zero so a rehearsal cannot quietly pass.

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
| `E2B_REHEARSAL_SPRAY=n` | object-count shape: `n` small objects (4 KiB) with p50/p95/p99 latencies, an inventory (`count`) of the prefix, a rewrite with `--cleanup`, and a second inventory proving the delete |
| `E2B_REHEARSAL_FAULT=1` | fault injection: overwrite one artifact with different bytes, then read it with the other version; the verdict is `ok` only if the tampering is *detected* (loud refusal or misread) |
| `E2B_REHEARSAL_NFS=1` | put every node's chunk cache on the VM's NFS export (`/mnt/nfs-cache`) instead of a local temp dir |
| `E2B_REHEARSAL_SOAK=k` | repeat the whole matrix `k` times (drift/soak) |

## Evidence (2026-09-19, dev VM, Silo)

- sequential matrix: green — 8/8 artifacts each way, v5 headers, 16/16 objects
  present on both sides
- fan-out: 3 concurrent nodes, versions mixed, every write ok and every
  cross-version read 8/8
- object-count: 1500 × 4 KiB sprayed at p50 18 ms / p95 34 ms / p99 50 ms; the
  inventory counted exactly 1500 objects (5.9 MiB); the prefix delete took
  1.7 s and the second inventory read 0
- fault injection: the tampered artifact was refused loudly ("magic number
  mismatch") in both soak rounds — never silently accepted
- NFS: caches land on `/mnt/nfs-cache/<run>/<node>` (105 files, 85 MiB in the
  first run)
- soak: 2 rounds, 28 `ok` outcomes, no errors or misreads
