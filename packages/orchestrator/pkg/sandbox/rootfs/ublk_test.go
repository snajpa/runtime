//go:build linux

package rootfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/ublk"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	errProviderDevice  = errors.New("provider device cleanup failed")
	errProviderManager = errors.New("provider manager cleanup failed")
)

type providerTestRootfs struct{}

func (providerTestRootfs) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func (providerTestRootfs) Size(context.Context) (int64, error) { return 4096, nil }
func (providerTestRootfs) Close() error                        { return nil }
func (providerTestRootfs) Slice(context.Context, int64, int64) ([]byte, error) {
	return nil, errors.New("not used")
}
func (providerTestRootfs) BlockSize() int64          { return 4096 }
func (providerTestRootfs) Header() *header.Header    { return &header.Header{} }
func (providerTestRootfs) SwapHeader(*header.Header) {}

var _ block.ReadonlyDevice = providerTestRootfs{}

type providerTestManager struct {
	closeErr   error
	openDevice ublkDevice
	openErr    error
}

func (m *providerTestManager) Open(context.Context, ublk.Backend, ublk.Options) (ublkDevice, error) {
	if m.openDevice != nil || m.openErr != nil {
		return m.openDevice, m.openErr
	}
	return nil, errors.New("not used")
}

func (m *providerTestManager) Close() error { return m.closeErr }

type providerTestDevice struct {
	closeErr  error
	release   ublk.ReleaseState
	releaseFn func() ublk.ReleaseState
	failure   error
	syncErr   error
	abortErr  error
}

func (d *providerTestDevice) Path() string                       { return "/dev/ublkb-test" }
func (d *providerTestDevice) Failure() error                     { return d.failure }
func (d *providerTestDevice) Sync(context.Context) error         { return d.syncErr }
func (d *providerTestDevice) Close(context.Context) error        { return d.closeErr }
func (d *providerTestDevice) Abort(context.Context, error) error { return d.abortErr }
func (d *providerTestDevice) ReleaseState() ublk.ReleaseState {
	if d.releaseFn != nil {
		return d.releaseFn()
	}
	return d.release
}

var _ ublkDevice = (*providerTestDevice)(nil)
var _ ublkManager = (*providerTestManager)(nil)

func newProviderTest(t *testing.T, manager *providerTestManager, device ublkDevice) *UblkProvider {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "rootfs.cow")
	cache, err := block.NewCache(4096, 4096, cachePath, false)
	if err != nil {
		t.Fatalf("create cache: %v", err)
	}

	return &UblkProvider{
		overlay:          block.NewOverlay(providerTestRootfs{}, cache),
		manager:          manager,
		device:           device,
		cachePath:        cachePath,
		ready:            utils.NewSetOnce[string](),
		finished:         make(chan struct{}),
		closeResultReady: make(chan struct{}),
	}
}

func TestUblkProviderFailedStartPublishesPathError(t *testing.T) {
	startErr := errors.New("ublk start failed")
	provider := newProviderTest(t, &providerTestManager{openErr: startErr}, nil)

	if err := provider.Start(context.Background()); !errors.Is(err, startErr) {
		t.Fatalf("Start() error = %v, want %v", err, startErr)
	}

	pathDone := make(chan error, 1)
	go func() {
		_, err := provider.Path()
		pathDone <- err
	}()
	select {
	case err := <-pathDone:
		if !errors.Is(err, startErr) {
			t.Fatalf("Path() error = %v, want %v", err, startErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Path() remained blocked after failed Start()")
	}
}

func TestUblkProviderClosePropagatesReleaseErrors(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{closeErr: errProviderManager}, &providerTestDevice{
		closeErr: errProviderDevice,
		release:  ublk.ReleaseProven,
	})

	err := provider.Close(context.Background())
	if !errors.Is(err, errProviderDevice) || !errors.Is(err, errProviderManager) {
		t.Fatalf("Close() error = %v, want both release errors", err)
	}
	if err = provider.Close(context.Background()); !errors.Is(err, errProviderDevice) || !errors.Is(err, errProviderManager) {
		t.Fatalf("repeated Close() error = %v, want sticky release errors", err)
	}
}

func TestUblkProviderRejectsFallbackWhenLocalCleanupFails(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{closeErr: errProviderManager}, nil)
	if err := provider.closeBeforeFallback(); !errors.Is(err, errProviderManager) {
		t.Fatalf("closeBeforeFallback() error = %v, want %v", err, errProviderManager)
	}
}

func TestUblkProviderCleanupFailureBlocksExport(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{closeErr: errProviderManager}, &providerTestDevice{
		closeErr: errProviderDevice,
		release:  ublk.ReleaseProven,
	})
	if err := provider.Close(context.Background()); err == nil {
		t.Fatal("Close() unexpectedly succeeded")
	}

	out, err := os.CreateTemp(t.TempDir(), "diff-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, err = provider.ExportDiff(context.Background(), out, func(context.Context) error { return nil })
	if err == nil || !errors.Is(err, errProviderManager) {
		t.Fatalf("ExportDiff() error = %v, want cleanup failure", err)
	}
}

func TestUblkProviderLateReleaseFailureStaysSticky(t *testing.T) {
	var released atomic.Bool
	provider := newProviderTest(t, &providerTestManager{closeErr: errProviderManager}, &providerTestDevice{
		closeErr: errProviderDevice,
		releaseFn: func() ublk.ReleaseState {
			if released.Load() {
				return ublk.ReleaseProven
			}
			return ublk.ReleasePending
		},
	})

	if err := provider.Close(context.Background()); !errors.Is(err, errProviderDevice) {
		t.Fatalf("initial Close() error = %v, want sticky device error", err)
	}
	released.Store(true)

	select {
	case <-provider.finished:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for late release")
	}

	if err := provider.Close(context.Background()); !errors.Is(err, errProviderManager) {
		t.Fatalf("late Close() error = %v, want manager release error", err)
	}

	out, err := os.CreateTemp(t.TempDir(), "diff-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err = provider.ExportDiff(context.Background(), out, func(context.Context) error { return nil }); !errors.Is(err, errProviderDevice) {
		t.Fatalf("late ExportDiff() error = %v, want sticky device error", err)
	}
}

func TestUblkProviderExportDiffCloseCallbackPublishesResult(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{}, &providerTestDevice{
		release: ublk.ReleaseProven,
	})
	out, err := os.CreateTemp(t.TempDir(), "diff-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	if _, err = provider.ExportDiff(context.Background(), out, func(ctx context.Context) error {
		return provider.Close(ctx)
	}); err != nil {
		t.Fatalf("ExportDiff() = %v, want successful callback cleanup", err)
	}
}

func TestUblkProviderPendingToImmediateReleasePublishesCloseResult(t *testing.T) {
	var stateChecks atomic.Int32
	provider := newProviderTest(t, &providerTestManager{closeErr: errProviderManager}, &providerTestDevice{
		releaseFn: func() ublk.ReleaseState {
			if stateChecks.Add(1) == 1 {
				return ublk.ReleasePending
			}
			return ublk.ReleaseProven
		},
	})
	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v, want caller result before late release", err)
	}

	out, err := os.CreateTemp(t.TempDir(), "diff-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err = provider.ExportDiff(context.Background(), out, func(context.Context) error { return nil }); !errors.Is(err, errProviderManager) {
		t.Fatalf("ExportDiff() error = %v, want published late cleanup error", err)
	}
}

func TestUblkProviderPrepareExportDiffTransfersCacheOwnership(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{}, &providerTestDevice{
		release: ublk.ReleaseProven,
	})
	cache, err := provider.PrepareExportDiff(context.Background(), provider.Close)
	if err != nil {
		t.Fatalf("PrepareExportDiff() = %v, want success", err)
	}
	if cache == nil {
		t.Fatal("PrepareExportDiff() returned nil cache")
	}
	provider.ejectedMu.Lock()
	owned := provider.ejected
	provider.ejectedMu.Unlock()
	if owned != nil {
		t.Fatal("provider retained ejected cache after successful handoff")
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("deferred owner Close() = %v, want success", err)
	}
	if err := cache.Close(); err == nil {
		t.Fatal("second cache Close() unexpectedly succeeded")
	}
}

func TestUblkProviderPrepareExportDiffCleanupFailureClosesRetainedCache(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{closeErr: errProviderManager}, &providerTestDevice{
		release: ublk.ReleaseProven,
	})
	cachePath := provider.cachePath
	if _, err := provider.PrepareExportDiff(context.Background(), provider.Close); !errors.Is(err, errProviderManager) {
		t.Fatalf("PrepareExportDiff() error = %v, want manager cleanup error", err)
	}
	provider.ejectedMu.Lock()
	owned := provider.ejected
	provider.ejectedMu.Unlock()
	if owned != nil {
		t.Fatal("provider retained cache after cleanup failure closed it")
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ejected cache path still exists after cleanup failure: %v", err)
	}
}

func TestUblkProviderSwapAndFoldSealedPreservesOverlayData(t *testing.T) {
	provider := newProviderTest(t, &providerTestManager{}, &providerTestDevice{
		release: ublk.ReleasePending,
	})
	defer provider.overlay.Close()

	want := bytes.Repeat([]byte{0x5a}, 4096)
	if _, err := provider.overlay.WriteAt(want, 0); err != nil {
		t.Fatalf("initial overlay WriteAt() = %v", err)
	}
	old, err := provider.SwapForBackgroundSeal(context.Background())
	if err != nil {
		t.Fatalf("SwapForBackgroundSeal() = %v, want success", err)
	}
	if old == nil {
		t.Fatal("SwapForBackgroundSeal() returned nil frozen cache")
	}
	freshPath := strings.TrimSuffix(provider.cachePath, ".cow") + "-seal1.cow"
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh live cache missing: %v", err)
	}

	detached, err := provider.FoldSealed(context.Background())
	if err != nil {
		t.Fatalf("FoldSealed() = %v, want success", err)
	}
	if detached != old {
		t.Fatal("FoldSealed() returned a cache other than the frozen cache")
	}
	if err := detached.Close(); err != nil {
		t.Fatalf("folded cache Close() = %v, want success", err)
	}

	got := make([]byte, len(want))
	if _, err := provider.overlay.ReadAt(context.Background(), got, 0); err != nil {
		t.Fatalf("overlay ReadAt() = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("folded overlay data differs from pre-swap data")
	}
}
