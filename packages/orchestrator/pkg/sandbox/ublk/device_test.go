//go:build linux

package ublk

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOptionsNormalized(t *testing.T) {
	t.Parallel()

	t.Run("fills in unset fields", func(t *testing.T) {
		t.Parallel()

		got, err := Options{}.normalized()
		if err != nil {
			t.Fatalf("normalized: %v", err)
		}

		// Discard is not defaulted: the zero value means "do not advertise
		// discard", and callers that want the defaults start from
		// DefaultOptions.
		want := DefaultOptions()
		want.Discard = false

		if got != want {
			t.Errorf("normalized = %+v, want %+v", got, want)
		}
	})

	t.Run("keeps explicit values", func(t *testing.T) {
		t.Parallel()

		want := Options{Queues: 4, QueueDepth: 32, MaxIOBufBytes: 1 << 20, BlockSize: 4096, Discard: false}

		got, err := want.normalized()
		if err != nil {
			t.Fatalf("normalized: %v", err)
		}
		if got != want {
			t.Errorf("normalized = %+v, want %+v", got, want)
		}
	})

	t.Run("rejects out of range values", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name string
			opts Options
		}{
			{"no queues", Options{Queues: -1}},
			{"too many queues", Options{Queues: ublkMaxNrQueues + 1}},
			{"no depth", Options{QueueDepth: -1}},
			{"too deep", Options{QueueDepth: ublkMaxQueueDepth + 1}},
			{"tiny buffer", Options{MaxIOBufBytes: 1024}},
			{"huge buffer", Options{MaxIOBufBytes: maxIOBufBytes + 4096}},
			{"odd block size", Options{BlockSize: 1024}},
			{"legacy block size", Options{BlockSize: 512}},
		} {
			if _, err := tc.opts.normalized(); err == nil {
				t.Errorf("%s: normalized accepted %+v", tc.name, tc.opts)
			}
		}
	})
}

// The commit result is the only place a backend failure can surface, so it has
// to carry the errno the kernel expects.
func TestErrnoResult(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want int32
	}{
		{"errno", unix.ENOSPC, -int32(unix.ENOSPC)},
		{"wrapped errno", fmt.Errorf("writing: %w", unix.EPERM), -int32(unix.EPERM)},
		{"plain error", errors.New("backend failed"), -int32(unix.EIO)},
		{"canceled", context.Canceled, -int32(unix.EINTR)},
		{"deadline", context.DeadlineExceeded, -int32(unix.ETIMEDOUT)},
	} {
		if got := errnoResult(tc.err); got != tc.want {
			t.Errorf("%s: errnoResult = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func newLifecycleTestDevice() *Device {
	mgr := &Manager{devices: make(map[*Device]struct{})}
	d := &Device{
		mgr:          mgr,
		cdevFD:       -1,
		blockFD:      -1,
		state:        deviceRunning,
		terminalDone: make(chan struct{}),
		cancel:       func() {},
	}
	mgr.devices[d] = struct{}{}

	return d
}

func TestAbortCauseIsStickyAcrossClose(t *testing.T) {
	t.Parallel()

	cause := errors.New("backend write failed")
	d := newLifecycleTestDevice()

	got := d.Abort(context.Background(), cause)
	if !errors.Is(got, cause) {
		t.Fatalf("Abort error = %v, want cause %v", got, cause)
	}
	if got := d.Close(context.Background()); !errors.Is(got, cause) {
		t.Fatalf("repeated Close error = %v, want sticky cause %v", got, cause)
	}
	if got := d.Failure(); !errors.Is(got, cause) {
		t.Fatalf("Failure = %v, want sticky cause %v", got, cause)
	}
}

func TestTerminalTimeoutCannotBecomeCleanAfterWorkerReturns(t *testing.T) {
	t.Parallel()

	d := newLifecycleTestDevice()
	d.state = deviceClosing
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	first := d.waitTerminal(ctx, d.terminalDone)
	var quarantine *QuarantineError
	if !errors.As(first, &quarantine) {
		t.Fatalf("timeout result = %v, want QuarantineError", first)
	}

	// A late worker completion must not overwrite the sticky timeout or close
	// the retained manager child as if the original Close had succeeded.
	d.runTerminal(true, nil)
	second := d.Close(context.Background())
	if !errors.As(second, &quarantine) {
		t.Fatalf("late Close result = %v, want sticky QuarantineError", second)
	}
	if got := d.ReleaseState(); got != ReleaseProven {
		t.Fatalf("ReleaseState = %d, want ReleaseProven", got)
	}
	if _, ok := d.mgr.devices[d]; ok {
		t.Fatal("proven-released device remained in manager ownership")
	}
}

func TestReleaseStateStartsPending(t *testing.T) {
	t.Parallel()

	if got := newLifecycleTestDevice().ReleaseState(); got != ReleasePending {
		t.Fatalf("initial ReleaseState = %d, want ReleasePending", got)
	}
}

func TestManagerRefusesActiveDeviceClose(t *testing.T) {
	t.Parallel()

	d := newLifecycleTestDevice()
	if err := d.mgr.Close(); !errors.Is(err, ErrActiveDevices) {
		t.Fatalf("Manager.Close = %v, want %v", err, ErrActiveDevices)
	}
}

type mismatchedBlockBackend struct{ fakeBackend }

func (mismatchedBlockBackend) BlockSize() int64 { return 512 }

func TestOpenValidationIsFallbackSafeBeforeSideEffects(t *testing.T) {
	t.Parallel()

	mgr := &Manager{}
	backend := &fakeBackend{data: make([]byte, 4096)}
	_, err := mgr.Open(context.Background(), backend, Options{BlockSize: 512})

	var openErr *OpenError
	if !errors.As(err, &openErr) {
		t.Fatalf("Open error = %v, want OpenError", err)
	}
	if !openErr.FallbackSafe() || openErr.Kind != OpenFailureNoSideEffect {
		t.Fatalf("Open error = %#v, want no-side-effect fallback result", openErr)
	}
}

func TestOpenRejectsBackendBlockMismatchBeforeSideEffects(t *testing.T) {
	t.Parallel()

	mgr := &Manager{}
	backend := &mismatchedBlockBackend{fakeBackend: fakeBackend{data: make([]byte, 4096)}}
	_, err := mgr.Open(context.Background(), backend, DefaultOptions())

	var openErr *OpenError
	if !errors.As(err, &openErr) || !openErr.FallbackSafe() {
		t.Fatalf("Open error = %v, want fallback-safe OpenError", err)
	}
}

func TestSyncRejectsConcurrentTerminalClose(t *testing.T) {
	t.Parallel()

	d := newLifecycleTestDevice()
	d.state = deviceClosing
	if err := d.Sync(context.Background()); !errors.Is(err, ErrDeviceClosing) {
		t.Fatalf("Sync error = %v, want %v", err, ErrDeviceClosing)
	}
}

func TestUncertainAddFailureRetainsDevice(t *testing.T) {
	t.Parallel()

	d := newLifecycleTestDevice()
	d.addAttempted.Store(true)

	got, err := d.openFailure(errors.New("ADD_DEV completion uncertain"))
	if got != d {
		t.Fatalf("openFailure device = %p, want retained %p", got, d)
	}
	var openErr *OpenError
	if !errors.As(err, &openErr) || openErr.FallbackSafe() || openErr.Kind != OpenFailureQuarantined {
		t.Fatalf("openFailure error = %#v, want quarantined OpenError", err)
	}
	if got := d.Failure(); got == nil {
		t.Fatal("retained uncertain ADD_DEV failure did not record Failure")
	}
}

func TestOpenCompletionDoesNotReopenClosingDevice(t *testing.T) {
	t.Parallel()

	d := newLifecycleTestDevice()
	d.state = deviceClosing
	d.openDone = make(chan struct{})
	d.finishOpen(nil)

	d.mu.Lock()
	state := d.state
	d.mu.Unlock()
	if state != deviceClosing {
		t.Fatalf("startup completion changed state to %d, want deviceClosing", state)
	}
}

func TestBlockCloseErrorQuarantinesRelease(t *testing.T) {
	closeErr := errors.New("close failed")
	originalClose := closeFD
	closeFD = func(int) error { return closeErr }
	t.Cleanup(func() { closeFD = originalClose })

	d := newLifecycleTestDevice()
	d.blockFD = 42
	d.runTerminal(false, nil)

	if got := d.ReleaseState(); got != ReleaseQuarantined {
		t.Fatalf("ReleaseState = %d, want ReleaseQuarantined", got)
	}
	if got := d.terminalResult(); !errors.Is(got, closeErr) {
		t.Fatalf("terminal result = %v, want close error %v", got, closeErr)
	}
}

func TestQueueUnmapErrorQuarantinesRelease(t *testing.T) {
	unmapErr := errors.New("unmap failed")
	originalMunmap := munmap
	munmap = func([]byte) error { return unmapErr }
	t.Cleanup(func() { munmap = originalMunmap })

	d := newLifecycleTestDevice()
	d.queueBufs = [][]byte{{1, 2, 3}}
	d.runTerminal(false, nil)

	if got := d.ReleaseState(); got != ReleaseQuarantined {
		t.Fatalf("ReleaseState = %d, want ReleaseQuarantined", got)
	}
	if got := d.terminalResult(); !errors.Is(got, unmapErr) {
		t.Fatalf("terminal result = %v, want unmap error %v", got, unmapErr)
	}
	if len(d.queueBufs) != 1 {
		t.Fatalf("queue buffers = %d, want retained failed mapping", len(d.queueBufs))
	}
}
