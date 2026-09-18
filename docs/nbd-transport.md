# NBD storage transport

The per-sandbox rootfs transport in production: one kernel NBD device per
sandbox, served in-process by the orchestrator
(`packages/orchestrator/pkg/sandbox/nbd`). It remains the default; the ublk
transport ([`ublk-transport.md`](ublk-transport.md)) is its successor and
shares the overlay, cache, export and fold semantics below the transport
(`rootfs.NewOverlayProvider` selects between them).

## Durability contract (declared)

- **No write cache is advertised.** `Open` connects without
  `NBD_FLAG_SEND_FLUSH`, and the dispatcher refuses flush requests, so the
  block layer completes an empty flush bio itself: a guest `fsync` is a
  host-local claim, and no flush request reaches the orchestrator.
- **The durability boundary is the pause/close quiesce barrier.** Teardown
  flushes the device's writeback before releasing the descriptor (`Flush`:
  fsync on the descriptor `Open` kept, then `BLKFLSBUF`), and a failed
  barrier is fatal to the pause/export -- logged with the device path and
  counted (`orchestrator.rootfs.overlay.release.failed`), never exported
  past.
- **Consequence.** A host failure between a guest `fsync` and the next pause
  can lose data the guest may have treated as durable. Propagating guest
  flushes end-to-end (advertising `NBD_FLAG_SEND_FLUSH` and answering flush
  requests) is deliberately not implemented: it would put a device round trip
  on every guest `fsync`, and no workload class has been named that needs it.
  If one is, it ships per host behind a connection flag, with measurements.

## Teardown budget and signals

- **Declared budget.** A teardown facing a backend that has stopped
  answering is bounded by the kernel ceiling `ioTimeout + deadconnTimeout`
  (90s + 30s today; `nbd/path_direct.go`). The flush is deliberately allowed
  to run to that ceiling rather than abandon writes the kernel already
  acknowledged to the guest.
- **Signals.** Through a live path the flush and close finish well under a
  second. The close watchdog warns at `deviceCloseWarnThreshold` (5s),
  tagging the step in progress; `orchestrator.nbd.device.close.slow` counts
  teardowns that crossed it; `orchestrator.nbd.device.close.duration`
  carries their full distribution; `orchestrator.nbd.device.flush` counts
  flush outcomes by stage.
- **Operating rule.** A teardown approaching the ceiling means a backend
  that is not answering: alert on the slow-close counter, not only on the
  duration tail.
- **Coverage.** `nbd/path_direct_close_stall_test.go` (the stall, the
  watchdog attribution and the declared bounds),
  `nbd/path_direct_flush_test.go` (the barrier's outcomes: failure
  surfaced; healthy flush silent and cheap), `rootfs/nbd_signal_test.go`
  (the pause/export propagation).

## Operations

- Host prerequisites (the `nbd` module, `nbds_max`, the udev rules) follow
  e2b's host-setup scripts (`embed/compose/scripts/host-setup.sh`); the dev
  VM provisions them (`nix/`).
- Device-touching tests take a cross-process lock and are safe to run
  alongside the rest of the suite.

## References

- `packages/orchestrator/pkg/sandbox/nbd` -- the transport implementation.
- `packages/orchestrator/pkg/sandbox/rootfs` -- the provider and the export
  path.
- [`ublk-transport.md`](ublk-transport.md) -- the successor transport.
