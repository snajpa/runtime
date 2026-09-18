//go:build linux

package nbd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bits-and-blooms/bitset"
	"github.com/stretchr/testify/require"
)

// TestPoolPopulateBackoffBounds pins S-32: the pool retry loop backs off
// exponentially from waitOnNBDError, never decreases, and never exceeds
// poolPopulateBackoffMax.
func TestPoolPopulateBackoffBounds(t *testing.T) {
	t.Parallel()

	if got := poolPopulateBackoff(0); got != waitOnNBDError {
		t.Fatalf("first retry wait = %v, want %v", got, waitOnNBDError)
	}

	previous := time.Duration(0)
	for attempt := range 40 {
		got := poolPopulateBackoff(attempt)
		if got > poolPopulateBackoffMax {
			t.Fatalf("attempt %d waits %v, beyond the cap %v", attempt, got, poolPopulateBackoffMax)
		}
		if got < previous {
			t.Fatalf("attempt %d waits %v, less than the previous %v", attempt, got, previous)
		}

		previous = got
	}

	if got := poolPopulateBackoff(40); got != poolPopulateBackoffMax {
		t.Fatalf("a starved pool must reach the cap %v, got %v", poolPopulateBackoffMax, got)
	}
}

// TestStatusPollBackoffBounds pins the same shape for the kernel-status poll
// loops: start at statusPollInitial, grow, cap at statusPollMax.
func TestStatusPollBackoffBounds(t *testing.T) {
	t.Parallel()

	if got := statusPollBackoff(0); got != statusPollInitial {
		t.Fatalf("first poll wait = %v, want %v", got, statusPollInitial)
	}

	previous := time.Duration(0)
	for attempt := range 30 {
		got := statusPollBackoff(attempt)
		if got > statusPollMax {
			t.Fatalf("attempt %d waits %v, beyond the cap %v", attempt, got, statusPollMax)
		}
		if got < previous {
			t.Fatalf("attempt %d waits %v, less than the previous %v", attempt, got, previous)
		}

		previous = got
	}

	if got := statusPollBackoff(30); got != statusPollMax {
		t.Fatalf("a stuck transition must reach the cap %v, got %v", statusPollMax, got)
	}
}

// TestReleaseWakesAWaitingPopulate pins that freeing a slot signals the wake
// channel Populate waits on, so a starved loop retries immediately instead of
// sleeping out its backoff (S-32).
func TestReleaseWakesAWaitingPopulate(t *testing.T) {
	t.Parallel()

	// The pool reads device state from sysBlockDir: a slot whose size reads 0
	// and whose pid file is absent is free.
	dir := t.TempDir()
	slotDir := filepath.Join(dir, "nbd5")
	require.NoError(t, os.MkdirAll(slotDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(slotDir, "size"), []byte("0\n"), 0o644))

	pool := &DevicePool{
		done:        make(chan struct{}),
		usedSlots:   bitset.New(16),
		slots:       make(chan DeviceSlot, 1),
		sysBlockDir: dir,
		wake:        make(chan struct{}, 1),
	}

	require.NoError(t, pool.release(t.Context(), 5))

	select {
	case <-pool.wake:
	default:
		t.Fatal("releasing a slot must wake a Populate loop waiting in its backoff")
	}
}
