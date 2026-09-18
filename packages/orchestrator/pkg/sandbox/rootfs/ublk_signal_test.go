//go:build linux

package rootfs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These pin the ublk release signal's contract, mirroring the NBD provider's
// S-19/S-30 tests after the review's P1 finding: a failed pause barrier must
// reject the export instead of shipping a silently incomplete diff.

// TestUblkProviderFinishedOperationsSignalNeverBlocks pins S-19: the release
// signal must not block on a full buffer (a second Close would hang teardown).
func TestUblkProviderFinishedOperationsSignalNeverBlocks(t *testing.T) {
	t.Parallel()

	p := &UblkProvider{finishedOperations: make(chan error, 1)}

	done := make(chan struct{})
	go func() {
		defer close(done)

		p.signalFinishedOperations(nil)
		p.signalFinishedOperations(nil)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the finished-operations signal blocked")
	}

	select {
	case <-p.finishedOperations:
	default:
		t.Fatal("the first signal must be delivered")
	}
}

// TestUblkProviderReleaseSignalCarriesTheBarrierOutcome pins S-30's shape for
// ublk: the signal must deliver the pause barrier's outcome, and a second
// Close must not overwrite it on the buffer-1 channel.
func TestUblkProviderReleaseSignalCarriesTheBarrierOutcome(t *testing.T) {
	t.Parallel()

	barrierErr := errors.New("backend is missing acknowledged writes")
	p := &UblkProvider{finishedOperations: make(chan error, 1)}

	p.signalFinishedOperations(barrierErr)
	p.signalFinishedOperations(nil)

	select {
	case got := <-p.finishedOperations:
		if !errors.Is(got, barrierErr) {
			t.Fatalf("the release signal must carry the barrier outcome, got %v", got)
		}
	default:
		t.Fatal("the first signal must be delivered")
	}
}

// TestUblkProviderAwaitOverlayReleaseSurfacesTheFailedFlush pins that a release
// carrying a failed device flush fails the export with the original error
// wrapped, while a clean release waits cleanly and an expired wait reports the
// timeout (the caller then reclaims the ejected cache).
func TestUblkProviderAwaitOverlayReleaseSurfacesTheFailedFlush(t *testing.T) {
	t.Parallel()

	barrierErr := errors.New("backend is missing acknowledged writes")
	p := &UblkProvider{finishedOperations: make(chan error, 1)}
	p.signalFinishedOperations(barrierErr)

	got := p.awaitOverlayRelease(t.Context())
	if !errors.Is(got, barrierErr) {
		t.Fatalf("a failed flush must surface the original error, got %v", got)
	}

	clean := &UblkProvider{finishedOperations: make(chan error, 1)}
	clean.signalFinishedOperations(nil)
	if err := clean.awaitOverlayRelease(t.Context()); err != nil {
		t.Fatalf("a clean release must not fail the export, got %v", err)
	}

	timedOut := &UblkProvider{finishedOperations: make(chan error, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := timedOut.awaitOverlayRelease(ctx); !errors.Is(err, errOverlayReleaseTimeout) {
		t.Fatalf("an expired wait must report the timeout, got %v", err)
	}
}

// TestBarrierOutcome pins the join the release signal carries, including
// reviewer0's case: the first sync succeeds but the device teardown fails, so
// the barrier must still report a failure and the export must refuse. The
// call-site wiring itself needs the real device and stays inspection-verified,
// exactly like the NBD provider's syncErr+mountCloseErr join.
func TestBarrierOutcome(t *testing.T) {
	t.Parallel()

	syncErr := errors.New("cow flush failed")
	closeErr := errors.New("device teardown failed")

	cases := map[string]struct {
		syncErr        error
		deviceCloseErr error
		wantErr        error
	}{
		"both steps complete":              {nil, nil, nil},
		"the first sync fails":             {syncErr, nil, syncErr},
		"first sync succeeds, close fails": {nil, closeErr, closeErr},
		"both steps fail":                  {syncErr, closeErr, syncErr},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := barrierOutcome(tc.syncErr, tc.deviceCloseErr)
			if tc.wantErr == nil {
				if got != nil {
					t.Fatalf("a completed barrier must report nil, got %v", got)
				}

				return
			}

			if !errors.Is(got, tc.wantErr) {
				t.Fatalf("the barrier outcome must surface the failed step, got %v", got)
			}
		})
	}

	// Both steps failing surfaces both errors, not only the first.
	both := barrierOutcome(syncErr, closeErr)
	if !errors.Is(both, syncErr) || !errors.Is(both, closeErr) {
		t.Fatalf("both failures must be joined, got %v", both)
	}
}
