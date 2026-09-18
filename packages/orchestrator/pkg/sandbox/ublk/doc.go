// Package ublk implements the userspace side of the Linux ublk driver: a
// block device whose I/O is served by this process over io_uring.
//
// The kernel exposes /dev/ublkbN to the system and forwards every request to
// a uring_cmd command on /dev/ublkcN, which this package picks up and
// completes. Device setup and teardown go through /dev/ublk-control.
//
// Only the subset the orchestrator needs is implemented: user-copy I/O
// (UBLK_F_USER_COPY), one io_uring per hardware queue, and the traditional
// per-I/O commands. Batch I/O, zero copy and user recovery are intentionally
// not implemented.
//
// The ABI mirrors include/uapi/linux/ublk_cmd.h and the userspace protocol of
// ublk-org/ubdsrv (lib/ublksrv.c).
package ublk
