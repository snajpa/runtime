//go:build linux

package ublk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// waitFlushBudget is what bounds the barrier a close or export performs: the
// flush itself observes no context, so the wait must have all three exits.
func TestWaitFlushBudget(t *testing.T) {
	t.Parallel()

	t.Run("returns the flush's result", func(t *testing.T) {
		t.Parallel()

		flushErr := errors.New("writeback failed")
		done := make(chan error, 1)
		done <- flushErr

		if err := waitFlushBudget(t.Context(), "/dev/ublkb0", flushGrace, done); !errors.Is(err, flushErr) {
			t.Fatalf("waitFlushBudget = %v, want %v", err, flushErr)
		}
	})

	t.Run("bounds a flush that never completes", func(t *testing.T) {
		t.Parallel()

		start := time.Now()
		err := waitFlushBudget(t.Context(), "/dev/ublkb0", 50*time.Millisecond, make(chan error))
		elapsed := time.Since(start)

		if !errors.Is(err, errBarrierBudget) {
			t.Fatalf("waitFlushBudget = %v, want errBarrierBudget", err)
		}
		if elapsed < 40*time.Millisecond {
			t.Fatalf("waitFlushBudget returned before the budget: %s", elapsed)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("waitFlushBudget was not bounded: %s", elapsed)
		}
	})

	t.Run("stops when the caller's context ends", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := waitFlushBudget(ctx, "/dev/ublkb0", flushGrace, make(chan error))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitFlushBudget = %v, want context.Canceled", err)
		}
	})
}

// The emergency path must not release a stuck task's descriptors under it, and
// must not leak them once it unwinds: the reaper is the handoff between those
// two facts.
func TestReapAfterOwnersReleasesResources(t *testing.T) {
	t.Parallel()

	ringFD, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/null: %v", err)
	}
	cdevFD, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/null: %v", err)
	}
	buf, err := unix.Mmap(-1, 0, unix.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		t.Fatalf("cannot map a queue buffer: %v", err)
	}

	d := &Device{
		owners:    []*queueOwner{{ring: &ioUring{fd: ringFD}}},
		queueBufs: [][]byte{buf},
		cdevFD:    cdevFD,
	}

	// A task that has not unwound yet keeps its resources.
	d.ownersDone.Add(1)

	reaped := make(chan struct{})
	go func() {
		d.reapAfterOwners()
		close(reaped)
	}()

	select {
	case <-reaped:
		t.Fatal("the reaper released resources while a queue task was still live")
	case <-time.After(50 * time.Millisecond):
	}

	d.ownersDone.Done()

	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaper did not finish after the queue tasks unwound")
	}

	if _, err := unix.FcntlInt(uintptr(ringFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("the ring descriptor was not closed: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(cdevFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("the character device was not closed: %v", err)
	}
	if d.queueBufs != nil {
		t.Fatalf("the queue buffers were not released: %#v", d.queueBufs)
	}
}

// The drain is the barrier's second half, so it needs the same two exits: end
// when the requests drain, and fail loudly when they cannot.
func TestWaitInFlightBudget(t *testing.T) {
	t.Parallel()

	d := &Device{}

	if err := d.waitInFlight(t.Context(), 50*time.Millisecond); err != nil {
		t.Fatalf("waitInFlight on an idle device = %v, want nil", err)
	}

	d.inFlight.Store(1)

	start := time.Now()
	err := d.waitInFlight(t.Context(), 50*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, errBarrierBudget) {
		t.Fatalf("waitInFlight = %v, want errBarrierBudget", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("waitInFlight was not bounded: %s", elapsed)
	}
}

// A flush that cannot take its own descriptor must fail the barrier loudly
// instead of falling back to the shared node descriptor, which teardown may
// close (and whose number could be reused) under the flush.
func TestSyncFailsWithoutPrivateDescriptor(t *testing.T) {
	t.Parallel()

	d := &Device{path: "/dev/ublkb0", blockFD: 1 << 30}
	d.started.Store(true)

	err := d.Sync(t.Context())

	if err == nil {
		t.Fatal("Sync succeeded without a private descriptor")
	}
	if !errors.Is(err, unix.EBADF) {
		t.Fatalf("Sync = %v, want an EBADF failure", err)
	}
	if !strings.Contains(err.Error(), "duplicating the node descriptor") {
		t.Fatalf("Sync = %v, want the duplicating failure named", err)
	}
}
