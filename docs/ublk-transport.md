# ublk storage transport

The sandbox rootfs is a per-sandbox copy-on-write cache over a read-only template
rootfs, exposed to Firecracker as a block device. The default transport is an
in-process NBD server (`pkg/sandbox/nbd`); this document covers the alternative
transport, `pkg/sandbox/ublk`, which serves the same cache through the Linux
`ublk` driver as `/dev/ublkbN`.

Status: implemented and tested behind the `ublk-rootfs` feature flag (default
off). The NBD transport stays the default and is unchanged; a host with the flag
on but no usable ublk driver logs the failure and falls back to NBD.
See `ARCHITECTURE.md` for where the transport sits in the node.

## Requirements

What the rootfs transport has to provide, from the orchestrator's side:

1. **A device node Firecracker opens.** The sandbox's Firecracker process gets a
   block device path; nothing else in the sandbox path may change.
2. **The overlay's semantics, unchanged.** The transport serves a
   `block.Device` (the COW overlay). Writes the guest has been told about must
   be in the backend before the export paths read the cache, `WriteZeroesAt`
   must back discard and write-zeroes, and a read of a block the cache does not
   hold must come from the template.
3. **A flush contract.** The export/pause paths need a barrier that surfaces
   writeback failures: writes the kernel could not deliver must fail the flush
   rather than silently vanish.
4. **Bounded teardown.** Closing a sandbox must stop serving I/O, complete or
   fail what is in flight, and delete the device — never strand a device, and
   never wait forever on a node that is still open.
5. **Performance** at least on par with NBD for the sandbox rootfs workload
   (sequential and random, read and write), with a per-host tunable for
   concurrency.
6. **Deployability.** Selectable per host, defaulting to the existing path, and
   able to fall back when the driver is not there. No new dependencies and no
   cgo: the orchestrator builds as a static host binary and runs as root.

## Design

### Kernel interface

Two interfaces, both io_uring based (no ioctls, no cgo):

- **Control plane** — `uring_cmd` on `/dev/ublk-control`, on an **SQE128** ring.
  The driver rejects control commands that arrive on a ring with standard SQEs
  (`IO_URING_F_SQE128`), and reads the command payload (`struct
  ublksrv_ctrl_cmd`, 32 bytes) directly out of the SQE at offset 48. Commands
  are matched by `_IOC_NR`, and with `UBLK_F_CMD_IOCTL_ENCODE` (which the driver
  sets) the ops are the ioctl-encoded `UBLK_U_CMD_*` values. The transport uses
  `ADD_DEV`, `SET_PARAMS`, `START_DEV`, `STOP_DEV` and `DEL_DEV_ASYNC` (with a
  synchronous `DEL_DEV` fallback).
- **Data plane** — one io_uring per hardware queue, `IORING_OP_URING_CMD` with
  the 16-byte `struct ublksrv_io_cmd` payload in the SQE command area (standard
  64-byte SQEs are enough there). `cmd_op` is the ioctl-encoded
  `UBLK_U_IO_FETCH_REQ` / `UBLK_U_IO_COMMIT_AND_FETCH_REQ`, and `user_data`
  packs the tag and command number. `sqe->fd` is the character device.

Protocol facts the implementation depends on (verified against
`include/uapi/linux/ublk_cmd.h` and `drivers/block/ublk_drv.c`):

- `ADD_DEV` passes `struct ublksrv_ctrl_dev_info` through `addr`/`len`;
  `dev_id = U32_MAX` asks the kernel to allocate one, and the kernel writes the
  allocated id and the negotiated flags back into that buffer.
- `SET_PARAMS` has to run before `START_DEV` and carries the
  `len`/`types`/`ublk_param_basic`/`ublk_param_discard` prefix. `max_sectors` is
  capped by `max_io_buf_bytes >> 9`; the discard type requires a non-zero
  granularity and `max_discard_segments == 1`.
- `START_DEV` carries the daemon pid in `data[0]` and the kernel only starts a
  device once **every** queue has all of its tags fetched.
- The kernel requires the `FETCH` and `COMMIT` commands of a `(queue, tag)` pair
  to come from the **same task** (`UBLK_F_PER_IO_DAEMON`, which the driver
  sets): the data plane is therefore one daemon task per queue, locked to its OS
  thread, and tags are never handed to another task.
- With `UBLK_F_USER_COPY` no buffer is registered: the kernel addresses request
  data by position, `ublk_pos() = 0x80000000 | qid << 41 | tag << 25 | offset`,
  and the daemon copies with `pread`/`pwrite` on `/dev/ublkcN`. The direction is
  the opposite of the request op: a kernel **READ** is served by writing the
  data into the tag's buffer (server buffer is the source), a kernel **WRITE**
  by reading it out.
- The per-queue command buffer is mmapped **read-only** at
  `qid * round_up(4096 * 24, page)`, `round_up(queue_depth * 24, page)` long; it
  holds one 24-byte descriptor per tag (`op_flags`, `nr_sectors`,
  `start_sector`, `addr`), written by the kernel before the fetch completes.
- A commit carries the byte count on success or a negative errno on failure
  (the kernel turns a zero-length read into `-EIO`); `STOP_DEV` completes
  outstanding commands with `UBLK_IO_RES_ABORT` (`-ENODEV`).
- `DEL_DEV` waits for the **last opener** of `/dev/ublkbN` to go away, so it
  cannot be used on the teardown path of a device that may still be open:
  `DEL_DEV_ASYNC` removes the device without that wait.

### Decisions

| # | Decision | Why |
|---|----------|-----|
| 1 | Control plane via uring_cmd, SQE128 ring | it is the only interface the driver implements, and it requires 128-byte SQEs |
| 2 | Data plane via per-queue io_uring uring_cmd | the mainline data path; one ring per queue, no cross-queue locking |
| 3 | `UBLK_F_USER_COPY` | no buffer registration or descriptor management; the orchestrator runs as root, which user copy requires |
| 4 | One queue per device by default, depth 64, both tunable | NBD-parity concurrency and a lower thread count; `ublk-queues` raises it where the workload wants it |
| 5 | Own io_uring wrapper over raw syscalls | no cgo, no dependency on a partial library surface |
| 6 | A boolean feature flag, NBD default | side-by-side testing and rollback in one deploy |
| 7 | Devices created on demand, deleted on teardown | no `nbds_max`-style ceiling and a drop-in replacement for the NBD provider's path |
| 8 | `DEL_DEV_ASYNC` on teardown | the synchronous delete waits for the last opener |
| 9 | No write cache advertised (`attrs` zero) | same device semantics as NBD-without-flush: the block layer completes flush requests itself, and `Sync()` is what reports writeback failures |

Deliberately not implemented: zero-copy, batch I/O (`UBLK_F_BATCH_IO`), user
recovery, unprivileged mode, zoned devices.

### Data path

Per request the flow is: the kernel writes the tag's descriptor and completes
the tag's pending fetch → the queue task reads the descriptor → the backend is
asked for the bytes (READ: backend → buffer → `pwrite` into the tag's region;
WRITE: `pread` from the region → backend; DISCARD/WRITE_ZEROES:
`WriteZeroesAt`) → the same task commits the result, which re-arms the tag.

Concurrency comes from the number of queues: one request per queue at a time,
with up to `queue_depth` requests in flight in the kernel per queue. That keeps
the per-tag task affinity rule trivially satisfied and needs no cross-thread
handoff of tags.

## Implementation

`packages/orchestrator/pkg/sandbox/ublk/`:

| file | responsibility |
|------|----------------|
| `doc.go` | package documentation: what is implemented, what is not |
| `uapi.go` | UAPI constants, field offsets, ioctl encoding, `ioPos`/`userData`, the `ublk_params` prefix |
| `uring.go` | minimal io_uring wrapper: setup, mmap (single-mmap and split SQ/CQ), SQE128 support, submission, completion |
| `control.go` | control-plane ring and the commands, one synchronous command at a time |
| `device.go` | device manager and device life cycle: add, params, start, sync, teardown, delete |
| `queue.go` | the per-queue daemon task: arm, fetch/commit loop, op mapping, user-copy transfers |
| `backend.go` | the `Backend` interface the data plane needs and the error→errno mapping |

Life cycle: `Manager.Open` adds the device, sets the parameters, opens
`/dev/ublkcN`, maps the command buffers, starts the queue tasks (which arm every
tag) and then sends `START_DEV` with the process pid; `Path` is the
`/dev/ublkbN` node Firecracker opens. `Sync` is the barrier: `fsync` on the node
the provider opened (that mapping is where writeback errors are recorded) plus
`BLKFLSBUF`, then wait for the requests the queues are serving. `Close` syncs,
releases our node descriptor, stops the device, waits for the queue tasks, then
closes the character device and deletes the device.

The teardown contract deserves its own note: **the queue tasks keep serving and
committing until the kernel aborts them**. `STOP_DEV` runs `del_gendisk`, which
waits for every request the kernel already dispatched to a queue; a task that
walked away from one would leave the stop — and the control command driving it —
hanging forever. A daemon killed in that state leaves the device wedged inside
the driver, holding the control mutex, so every later control command (including
`ADD_DEV` for the next sandbox) blocks until the machine reboots.
`TestCloseWithInFlightIO` covers exactly that window.

Integration: `pkg/sandbox/rootfs/ublk.go` is `UblkProvider`, the sibling of
`NBDProvider` — same overlay, cache, export and fold semantics, different
transport — and `rootfs.NewOverlayProvider` selects between them. The sandbox
create and resume paths go through that one selection point. Feature flags:
`ublk-rootfs` (off), `ublk-queues` (1), `ublk-queue-depth` (64).

## Operations

- **Host prerequisite**: the `ublk_drv` module and `/dev/ublk-control`
  (Ubuntu 24.04's HWE kernel has both; e2b's prod floor of 6.8/6.10 should be
  verified per fleet image). The module is not loaded by the host-setup scripts
  today; loading it is a per-host prerequisite for turning the flag on.
- **Selecting it**: enable `ublk-rootfs` for the host. Without the driver the
  node logs the failure and keeps using NBD.
- **Stale devices**: a crashed orchestrator can leave `/dev/ublkbN` behind. The
  devices are inert (I/O to them fails) and new devices get new ids; a device
  whose daemon died with a request in flight cannot be deleted at all — the
  delete waits for that request — so recovering it needs a reboot. The transport
  no longer produces that state itself (see the teardown contract).
- **Rollback**: turn the flag off; existing sandboxes finish on the transport
  they started with, because the device's life is tied to its provider.

## Testing

- Unit tests (host, no root): ABI layout, command encoding, params, position and
  user-data packing, ring helpers, option validation, request mapping.
- Root-gated integration (dev VM): device life cycle, multi-queue concurrency,
  backend errors surfacing as I/O errors, close with the node still open, close
  with I/O in flight, multiple devices, fio data integrity (verified I/O
  re-verified against the backing file after teardown), a filesystem on the
  device, and a direct-I/O backend comparison.
- Guest tests: a real Firecracker guest boots from a ublk device as its root
  disk, writes through a second raw ublk device, and survives a pause/resume
  (snapshot create + load) with the same devices serving it.
- Provider level: the overlay life cycle behind the ublk device (flush lands in
  the frozen cache after `SwapForBackgroundSeal`, post-swap writes stay out of
  it, `FoldSealed` detaches it, `ExportDiffInPlace` works while the device keeps
  serving).

The kernel-touching tests build on the host and run inside the dev VM; see
`nix/README.md` for the prerequisites (busybox-static, a public kernel and
Firecracker binary, `/dev/kvm`).

## Benchmarks

Same backend on both sides (one overlay over one writable cache, served once as
`/dev/ublkbN` and once as `/dev/nbdX` by the production provider paths), fio
`libaio`, `direct=1`, `iodepth=16`, `--time_based --runtime=10s`, `size=256m`, on
a 16 vCPU dev VM:

| workload | ublk 1 queue | ublk 4 queues | NBD (1 connection) |
|----------|--------------|---------------|--------------------|
| seq-write 1m | 1668.7 MB/s (1591 iops) | 1943.1 MB/s (1853 iops) | 241.7 MB/s (231 iops) |
| seq-read 1m | 1636.5 MB/s (1561 iops) | 1615.0 MB/s (1540 iops) | 548.7 MB/s (523 iops) |
| rand-write 4k | 140.7 MB/s (34358 iops) | 150.5 MB/s (36740 iops) | 52.8 MB/s (12897 iops) |
| rand-read 4k | 149.9 MB/s (36595 iops) | 150.3 MB/s (36702 iops) | 57.0 MB/s (13924 iops) |

`packages/orchestrator/benchmarks/transport_benchmark_test.go` reproduces the
table (`-bench BenchmarkTransportThroughput -benchtime=1x`, as root, in the dev
VM). The numbers are one VM and one run each; the NBD side is the in-repo
userspace server over the same cache, so the comparison is between transports,
not storage backends.

## References

- `Documentation/block/ublk.rst`, `include/uapi/linux/ublk_cmd.h`,
  `drivers/block/ublk_drv.c` in the Linux tree
- `ublk-org/ubdsrv` (libublksrv) for the userspace protocol
- `pkg/sandbox/ublk`, `pkg/sandbox/rootfs/ublk.go`,
  `pkg/sandbox/{nbd,block}` in this repository
