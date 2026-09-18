//go:build linux

package rootfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

type NBDProvider struct {
	overlay      *block.Overlay
	mnt          *nbd.DirectPathMount
	featureFlags *featureflags.Client

	ready *utils.SetOnce[string]

	blockSize int64
	// cachePath is the path of the initial writable cache; fresh caches created
	// by SwapForBackgroundSeal derive a unique path from it.
	cachePath string
	sealGen   atomic.Int64

	finishedOperations chan error
	devicePool         *nbd.DevicePool
}

func NewNBDProvider(ctx context.Context, rootfs block.ReadonlyDevice, cachePath string, devicePool *nbd.DevicePool, featureFlags *featureflags.Client) (Provider, error) {
	size, err := rootfs.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting device size: %w", err)
	}

	blockSize := rootfs.BlockSize()

	cache, err := block.NewCache(size, blockSize, cachePath, false)
	if err != nil {
		return nil, fmt.Errorf("error creating cache: %w", err)
	}

	overlay := block.NewOverlay(rootfs, cache)

	mnt := nbd.NewDirectPathMount(overlay, devicePool, featureFlags)

	return &NBDProvider{
		mnt:                mnt,
		overlay:            overlay,
		featureFlags:       featureFlags,
		ready:              utils.NewSetOnce[string](),
		finishedOperations: make(chan error, 1),
		blockSize:          blockSize,
		cachePath:          cachePath,
		devicePool:         devicePool,
	}, nil
}

func (o *NBDProvider) Start(ctx context.Context) error {
	deviceIndex, err := o.mnt.Open(ctx)
	if err != nil {
		return o.ready.SetError(fmt.Errorf("error opening overlay file: %w", err))
	}

	return o.ready.SetValue(nbd.GetDevicePath(deviceIndex))
}

// ejectAndStopSandbox detaches the writable cache from the overlay, stops the
// sandbox and waits for the overlay device to be released, returning the ejected
// (now standalone, frozen) cache. The caller owns the returned cache and must
// Close it. Shared by the synchronous ExportDiff and the deferred
// PrepareExportDiff. A release that reports a failed device flush fails here
// (S-30): the caller must not export a cache whose backend was still missing
// writes the guest was told had landed.
func (o *NBDProvider) ejectAndStopSandbox(
	ctx context.Context,
	closeSandbox func(ctx context.Context) error,
) (*block.Cache, error) {
	cache, err := o.overlay.EjectCache()
	if err != nil {
		return nil, fmt.Errorf("error ejecting cache: %w", err)
	}

	// the error is already logged in go routine in SandboxCreate handler
	go func() {
		err := closeSandbox(ctx)
		if err != nil {
			logger.L().Error(ctx, "error stopping sandbox on cow export", zap.Error(err))
		}
	}()

	releaseErr := o.awaitOverlayRelease(ctx)
	if errors.Is(releaseErr, errOverlayReleaseTimeout) {
		// Close the cache to avoid leaking the mmaped memory. Log an error
		// if that failed
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, releaseErr
	}
	if releaseErr != nil {
		// The device was released, but its pause barrier reported a failure:
		// the backend is missing writes the guest was told had landed, so an
		// exported diff would be silently incomplete. Fail the pause/export
		// loudly instead of exporting it (S-30, REQ-E1).
		o.reportOverlayReleaseFailure(ctx, releaseErr)

		// The cache was already detached from the overlay (EjectCache) and the
		// overlay skips ejected caches on Close, so reclaim it here — nobody
		// else owns it and a failed pause must not leak the mapping/file.
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, releaseErr
	}

	telemetry.ReportEvent(ctx, "sandbox stopped")

	return cache, nil
}

func (o *NBDProvider) ExportDiff(
	ctx context.Context,
	out *os.File,
	closeSandbox func(ctx context.Context) error,
) (*header.DiffMetadata, error) {
	ctx, span := tracer.Start(ctx, "cow-export")
	defer span.End()

	cache, err := o.ejectAndStopSandbox(ctx, closeSandbox)
	if err != nil {
		return nil, err
	}

	m, err := cache.ExportToDiff(ctx, out)
	if err != nil {
		// Close the cache to avoid leaking the mmaped memory. Log an error
		// if that failed
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, fmt.Errorf("error exporting cache: %w", err)
	}

	telemetry.ReportEvent(ctx, "cache exported")

	err = cache.Close()
	if err != nil {
		return nil, fmt.Errorf("error closing cache: %w", err)
	}

	return m, nil
}

// PrepareExportDiff ejects the writable cache, stops the sandbox and waits for
// the overlay device to be released, then returns the frozen ejected cache
// WITHOUT reflinking it. The caller reflinks it into a diff in the background and
// Closes it, so a pause returns without paying the reflink stall.
func (o *NBDProvider) PrepareExportDiff(
	ctx context.Context,
	closeSandbox func(ctx context.Context) error,
) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-export-prepare")
	defer span.End()

	return o.ejectAndStopSandbox(ctx, closeSandbox)
}

// ExportDiffInPlace exports the NBD cache into `out` without closing/destroying
// the underlying cache, so a sandbox that resumes in place keeps running on it.
func (o *NBDProvider) ExportDiffInPlace(
	ctx context.Context,
	out *os.File,
) (*header.DiffMetadata, error) {
	ctx, span := tracer.Start(
		ctx,
		"cow-export",
		trace.WithAttributes(attribute.Bool("in-place", true)),
	)
	defer span.End()

	if err := o.sync(ctx); err != nil {
		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}

	return o.overlay.ExportDiffInPlace(ctx, out)
}

// SwapForBackgroundSeal flushes the NBD device (so all in-flight writes land in
// the current cache), then swaps a fresh empty cache onto the overlay and returns
// the previous cache. The returned cache is frozen — new guest writes go to the
// fresh cache — so the caller can reflink/export it in the background while the
// VM resumes. Only the device flush stays on the critical path; the reflink is
// deferred.
func (o *NBDProvider) SwapForBackgroundSeal(ctx context.Context) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-swap-for-seal")
	defer span.End()

	if err := o.sync(ctx); err != nil {
		recordSealSwap(ctx, err)

		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}

	size, err := o.overlay.Size(ctx)
	if err != nil {
		recordSealSwap(ctx, err)

		return nil, fmt.Errorf("getting overlay size: %w", err)
	}

	gen := o.sealGen.Add(1)
	// The fresh live cache must keep the ".cow" SUFFIX: storage's
	// SandboxFileGlobs — the single source of truth for startup reclaim —
	// matches "rootfs-*-*.cow", and after the first fold this file is the
	// ONLY rootfs COW on disk (the fold closes and unlinks the original).
	// A "….cow.sealN" name would survive every unclean orchestrator exit
	// as an invisible, sandbox-cache-sized leak.
	freshPath := fmt.Sprintf("%s-seal%d.cow", strings.TrimSuffix(o.cachePath, ".cow"), gen)
	fresh, err := block.NewCache(size, o.blockSize, freshPath, false)
	if err != nil {
		recordSealSwap(ctx, err)

		return nil, fmt.Errorf("creating fresh cache: %w", err)
	}

	old, err := o.overlay.SwapCache(fresh)
	if err != nil {
		recordSealSwap(ctx, err)

		return nil, errors.Join(fmt.Errorf("swapping cache: %w", err), fresh.Close())
	}

	recordSealSwap(ctx, nil)

	return old, nil
}

// FoldSealed folds the sealing cache into the live writable cache and detaches it
// for closing. See block.Overlay.FoldSealing.
func (o *NBDProvider) FoldSealed(ctx context.Context) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-fold-sealed")
	defer span.End()

	start := time.Now()
	sealed, err := o.overlay.FoldSealing()
	recordSealFold(ctx, time.Since(start), err)

	return sealed, err
}

// signalFinishedOperations publishes the overlay-release signal, carrying the
// pause-barrier outcome (nil when the device flush succeeded, the flush error
// otherwise), without ever blocking: Close may run twice (or race the eject
// waiter), and a second blocking send on the buffer-1 channel would hang
// teardown forever (S-19, INV-5). The first signal is the one a waiter
// receives, so the outcome of the Close that released the device is the one
// enforced.
func (o *NBDProvider) signalFinishedOperations(barrierErr error) {
	select {
	case o.finishedOperations <- barrierErr:
	default:
	}
}

// errOverlayReleaseTimeout reports that the wait for the overlay-release
// signal ended because the caller's ctx expired rather than because the device
// was released.
var errOverlayReleaseTimeout = errors.New("timeout waiting for overlay device to be released")

// awaitOverlayRelease waits for the release signal Close publishes and returns
// the pause-barrier outcome it carries: nil when the device flush succeeded,
// the flush failure when the backend is missing writes the guest was told had
// landed, or errOverlayReleaseTimeout when ctx expired first (the caller then
// closes the ejected cache and abandons the export).
func (o *NBDProvider) awaitOverlayRelease(ctx context.Context) error {
	select {
	case barrierErr := <-o.finishedOperations:
		if barrierErr != nil {
			return fmt.Errorf("overlay device released with a failed flush: %w", barrierErr)
		}

		return nil
	case <-ctx.Done():
		return errOverlayReleaseTimeout
	}
}

// reportOverlayReleaseFailure records a release that carried a failed device
// flush: the export fails instead of building a silently incomplete diff, and
// the counter makes the rate visible: each count is a pause that failed loudly
// where it previously logged and exported anyway.
func (o *NBDProvider) reportOverlayReleaseFailure(ctx context.Context, releaseErr error) {
	devicePath, pathErr := o.Path()
	if pathErr != nil {
		devicePath = "unknown"
	}

	logger.L().Error(ctx, "overlay device released with a failed flush; failing the export",
		zap.String("device_path", devicePath),
		zap.Error(releaseErr),
	)

	overlayReleaseFailureCounter.Add(ctx, 1)
}

func (o *NBDProvider) Close(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "cow-close")
	defer span.End()

	var errs []error

	syncErr := o.sync(ctx)
	if syncErr != nil {
		errs = append(errs, fmt.Errorf("error flushing cow device: %w", syncErr))
	}

	mountCloseErr := o.mnt.Close(ctx)
	if mountCloseErr != nil {
		errs = append(errs, fmt.Errorf("error closing overlay mount: %w", mountCloseErr))
	}

	// Publish the barrier outcome with the release signal: the export that
	// waits for the device to be released must not build a diff from a backend
	// that is missing writes the guest was told had landed (S-30).
	o.signalFinishedOperations(errors.Join(syncErr, mountCloseErr))

	err := o.overlay.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing overlay cache: %w", err))
	}

	logger.L().Info(ctx, "overlay device released")

	return errors.Join(errs...)
}

func (o *NBDProvider) Path() (string, error) {
	return o.ready.Wait()
}

func (o *NBDProvider) sync(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "sync")
	defer span.End()

	if _, err := o.Path(); err != nil {
		return fmt.Errorf("failed to get cow path: %w", err)
	}

	return o.mnt.Flush(ctx)
}
