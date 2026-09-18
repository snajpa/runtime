//go:build linux

package rootfs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestNBDProviderFinishedOperationsSignalNeverBlocks pins S-19: the release
// signal must not block on a full buffer (a second Close would hang teardown).
func TestNBDProviderFinishedOperationsSignalNeverBlocks(t *testing.T) {
	t.Parallel()

	p := &NBDProvider{finishedOperations: make(chan error, 1)}

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

// TestNBDProviderReleaseSignalCarriesTheBarrierOutcome pins S-30: the release
// signal must deliver the pause barrier's outcome, and a second Close must not
// overwrite it on the buffer-1 channel.
func TestNBDProviderReleaseSignalCarriesTheBarrierOutcome(t *testing.T) {
	t.Parallel()

	barrierErr := errors.New("backend is missing acknowledged writes")
	p := &NBDProvider{finishedOperations: make(chan error, 1)}

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

// TestNBDProviderAwaitOverlayReleaseSurfacesTheFailedFlush pins that a release
// carrying a failed device flush fails the pause/export with the original error
// wrapped, while a clean release waits cleanly.
func TestNBDProviderAwaitOverlayReleaseSurfacesTheFailedFlush(t *testing.T) {
	t.Parallel()

	barrierErr := errors.New("sync NBD device: input/output error")

	failed := &NBDProvider{finishedOperations: make(chan error, 1)}
	failed.signalFinishedOperations(barrierErr)

	err := failed.awaitOverlayRelease(t.Context())
	if err == nil {
		t.Fatal("a release that carried a failed flush must fail the wait")
	}
	if !errors.Is(err, barrierErr) {
		t.Fatalf("the wait must surface the flush failure, got %v", err)
	}

	clean := &NBDProvider{finishedOperations: make(chan error, 1)}
	clean.signalFinishedOperations(nil)

	if err := clean.awaitOverlayRelease(t.Context()); err != nil {
		t.Fatalf("a clean release must wait cleanly, got %v", err)
	}
}

// TestNBDProviderAwaitOverlayReleaseTimesOutWithTheCallerContext pins that the
// wait still honours ctx, reporting the timeout error the eject path handles
// by closing the ejected cache.
func TestNBDProviderAwaitOverlayReleaseTimesOutWithTheCallerContext(t *testing.T) {
	t.Parallel()

	p := &NBDProvider{finishedOperations: make(chan error, 1)}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := p.awaitOverlayRelease(ctx); !errors.Is(err, errOverlayReleaseTimeout) {
		t.Fatalf("an expired ctx must report the release timeout, got %v", err)
	}
}

// TestNBDProviderFailedBarrierReclaimsEjectedCache pins reviewer-0's P2 case:
// a release signal carrying a failed pause barrier must fail the export AND
// reclaim the cache EjectCache already detached from the overlay — the overlay
// skips ejected caches on Close and the caller never receives the pointer, so
// nothing else can close it (S-30).
func TestNBDProviderFailedBarrierReclaimsEjectedCache(t *testing.T) {
	t.Parallel()

	const size = 8 << 20

	source, err := testutils.NewZeroDevice(size, header.RootfsBlockSize)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	cache, err := block.NewCache(size, header.RootfsBlockSize, filepath.Join(t.TempDir(), "rootfs.cow"), false)
	require.NoError(t, err)

	ready := utils.NewSetOnce[string]()
	require.NoError(t, ready.SetError(errors.New("provider not started")))

	p := &NBDProvider{
		overlay:            block.NewOverlay(source, cache),
		ready:              ready,
		finishedOperations: make(chan error, 1),
	}

	barrierErr := errors.New("sync NBD device: input/output error")
	p.signalFinishedOperations(barrierErr)

	got, err := p.ejectAndStopSandbox(t.Context(), func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("a release carrying a failed barrier must fail the export")
	}
	if !errors.Is(err, barrierErr) {
		t.Fatalf("the failure must preserve the flush error, got %v", err)
	}
	if got != nil {
		t.Fatal("a failed export must not hand the ejected cache to the caller")
	}

	// The detached cache was reclaimed: the mapping is unmapped and the file
	// removed, so every accessor reports the close (reviewer-0's probe saw
	// Size still succeed here).
	if _, err := cache.Size(); err == nil {
		t.Fatal("the ejected cache must be closed on the failed-release path")
	}
}
