//go:build linux

package rootfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/ublk"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// ublkDeviceBlockSize is what the ublk device advertises, the same logical
// block size the NBD transport sets on its device.
const ublkDeviceBlockSize = 4096

// UblkProvider is the NBD transport's counterpart on ublk: the same overlay and
// writable cache, served to Firecracker as /dev/ublkbN instead of /dev/nbdX.
//
// The life cycle mirrors NBDProvider on purpose: Start opens and starts the
// device, Path is what the sandbox hands to Firecracker, the export paths sync
// the device before they read the cache, and Close flushes, deletes the device
// and releases the overlay. The difference is the transport: NBD owns a pooled
// kernel device and dispatcher goroutines, while the ublk device only exists
// while this provider's sandbox does, and its data path is the ublk package's
// queue tasks.
type UblkProvider struct {
	overlay      *block.Overlay
	featureFlags *featureflags.Client
	manager      *ublk.Manager

	ready *utils.SetOnce[string]

	blockSize int64
	// cachePath is the path of the initial writable cache; fresh caches created
	// by SwapForBackgroundSeal derive a unique path from it.
	cachePath string
	sealGen   atomic.Int64

	// finishedOperations carries the pause-barrier outcome of the Close that
	// released the device (nil when the flush succeeded, the flush error
	// otherwise) so the export paths fail loudly instead of shipping a
	// silently incomplete diff (S-30 semantics, mirrored from the NBD
	// provider after the review's ublk barrier finding).
	finishedOperations chan error

	mu     sync.Mutex
	device *ublk.Device
	closed bool
}

func NewUblkProvider(ctx context.Context, rootfs block.ReadonlyDevice, cachePath string, featureFlags *featureflags.Client) (Provider, error) {
	size, err := rootfs.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting device size: %w", err)
	}

	blockSize := rootfs.BlockSize()

	cache, err := block.NewCache(size, blockSize, cachePath, false)
	if err != nil {
		return nil, fmt.Errorf("error creating cache: %w", err)
	}

	// One control plane per provider keeps the device's lifetime obvious: the
	// ring goes away with the sandbox it served.
	manager, err := ublk.NewManager()
	if err != nil {
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, fmt.Errorf("error opening the ublk control device: %w", err)
	}

	return &UblkProvider{
		overlay:            block.NewOverlay(rootfs, cache),
		featureFlags:       featureFlags,
		manager:            manager,
		ready:              utils.NewSetOnce[string](),
		finishedOperations: make(chan error, 1),
		blockSize:          blockSize,
		cachePath:          cachePath,
	}, nil
}

func (o *UblkProvider) Start(ctx context.Context) error {
	device, err := o.manager.Open(ctx, o.overlay, o.options(ctx))
	if err != nil {
		return o.ready.SetError(fmt.Errorf("error opening ublk device: %w", err))
	}

	o.mu.Lock()
	o.device = device
	o.mu.Unlock()

	return o.ready.SetValue(device.Path())
}

func (o *UblkProvider) options(ctx context.Context) ublk.Options {
	opts := ublk.DefaultOptions()
	opts.BlockSize = ublkDeviceBlockSize

	if queues := o.featureFlags.IntFlag(ctx, featureflags.UblkQueuesFlag); queues > 0 {
		opts.Queues = queues
	}

	if depth := o.featureFlags.IntFlag(ctx, featureflags.UblkQueueDepthFlag); depth > 0 {
		opts.QueueDepth = depth
	}

	return opts
}

// device returns the started device, or nil if the provider never started one.
func (o *UblkProvider) startedDevice() *ublk.Device {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.device
}

// ejectAndStopSandbox detaches the writable cache from the overlay, stops the
// sandbox and waits for the overlay device to be released, returning the ejected
// (now standalone, frozen) cache. The caller owns the returned cache and must
// Close it. Shared by the synchronous ExportDiff and the deferred
// PrepareExportDiff.
func (o *UblkProvider) ejectAndStopSandbox(
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
		// exported diff would be silently incomplete. Reclaim the detached
		// cache (nobody owns it on this path) and fail the pause/export
		// loudly instead of exporting it (S-30, REQ-E1).
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		o.reportOverlayReleaseFailure(ctx, releaseErr)

		return nil, releaseErr
	}
	telemetry.ReportEvent(ctx, "sandbox stopped")

	return cache, nil
}

func (o *UblkProvider) ExportDiff(
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
func (o *UblkProvider) PrepareExportDiff(
	ctx context.Context,
	closeSandbox func(ctx context.Context) error,
) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-export-prepare")
	defer span.End()

	return o.ejectAndStopSandbox(ctx, closeSandbox)
}

// ExportDiffInPlace exports the cache into `out` without closing/destroying the
// underlying cache, so a sandbox that resumes in place keeps running on it.
func (o *UblkProvider) ExportDiffInPlace(
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

// SwapForBackgroundSeal flushes the device (so all in-flight writes land in the
// current cache), then swaps a fresh empty cache onto the overlay and returns the
// previous cache. The returned cache is frozen — new guest writes go to the
// fresh cache — so the caller can reflink/export it in the background while the
// VM resumes. Only the device flush stays on the critical path; the reflink is
// deferred.
func (o *UblkProvider) SwapForBackgroundSeal(ctx context.Context) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-swap-for-seal")
	defer span.End()

	if err := o.sync(ctx); err != nil {
		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}

	size, err := o.overlay.Size(ctx)
	if err != nil {
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
		return nil, fmt.Errorf("creating fresh cache: %w", err)
	}

	old, err := o.overlay.SwapCache(fresh)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("swapping cache: %w", err), fresh.Close())
	}

	return old, nil
}

// FoldSealed folds the sealing cache into the live writable cache and detaches it
// for closing. See block.Overlay.FoldSealing.
func (o *UblkProvider) FoldSealed(ctx context.Context) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-fold-sealed")
	defer span.End()
	_ = ctx

	return o.overlay.FoldSealing()
}

func (o *UblkProvider) Close(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "cow-close")
	defer span.End()

	var errs []error

	var err error
	var barrierErr error
	var deviceCloseErr error

	// A provider that never started a device has nothing to flush; syncing
	// anyway would report a spurious "no ublk device to flush" on error paths.
	if o.startedDevice() != nil {
		err = o.sync(ctx)
		if err != nil {
			barrierErr = err
			errs = append(errs, fmt.Errorf("error flushing cow device: %w", err))
		}
	}

	o.mu.Lock()
	device := o.device
	o.device = nil
	alreadyClosed := o.closed
	o.closed = true
	o.mu.Unlock()

	if !alreadyClosed {
		if device != nil {
			if err := device.Close(ctx); err != nil {
				deviceCloseErr = err
				errs = append(errs, fmt.Errorf("error closing ublk device: %w", err))
			}
		}

		if err := o.manager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing ublk control device: %w", err))
		}

		// The device is gone, so nothing can reach the cache any more; this is
		// the point the export paths wait for before they read it.
		// The barrier is "flush and teardown completed", not just the first
		// sync: device.Close() performs another Sync and the STOP/delete/owner
		// teardown, so its failure also means the release did not complete.
		// Mirrors the NBD provider's syncErr+mountCloseErr join (S-30).
		o.signalFinishedOperations(barrierOutcome(barrierErr, deviceCloseErr))
	}

	err = o.overlay.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing overlay cache: %w", err))
	}

	logger.L().Info(ctx, "overlay device released")

	return errors.Join(errs...)
}

// barrierOutcome joins a pause barrier's steps into the outcome the release
// signal carries: the first sync's error and the ublk device teardown's error,
// either of which means the release did not complete (the barrier is "flush
// and teardown completed"). Extracted from Close so the join is pinned by a
// test; the call-site wiring itself needs the real device and stays
// inspection-verified, exactly like the NBD provider's syncErr+mountCloseErr
// join.
func barrierOutcome(syncErr, deviceCloseErr error) error {
	return errors.Join(syncErr, deviceCloseErr)
}

// signalFinishedOperations publishes the overlay-release signal, carrying the
// pause-barrier outcome (nil when the device flush succeeded, the flush error
// otherwise), without ever blocking: Close may run twice (or race the eject
// waiter), and a second blocking send on the buffer-1 channel would hang
// teardown forever (S-19, INV-5). The first signal is the one a waiter
// receives, so the outcome of the Close that released the device is enforced.
func (o *UblkProvider) signalFinishedOperations(barrierErr error) {
	select {
	case o.finishedOperations <- barrierErr:
	default:
	}
}

// awaitOverlayRelease waits for the release signal Close publishes and returns
// the pause-barrier outcome it carries: nil when the device flush succeeded,
// the flush failure when the backend is missing writes the guest was told had
// landed, or errOverlayReleaseTimeout when ctx expired first (the caller then
// closes the ejected cache and abandons the export).
func (o *UblkProvider) awaitOverlayRelease(ctx context.Context) error {
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
// the counter makes the rate visible (mirrored from the NBD provider, S-30).
func (o *UblkProvider) reportOverlayReleaseFailure(ctx context.Context, releaseErr error) {
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

func (o *UblkProvider) Path() (string, error) {
	return o.ready.Wait()
}

func (o *UblkProvider) sync(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "sync")
	defer span.End()

	if _, err := o.Path(); err != nil {
		return fmt.Errorf("failed to get cow path: %w", err)
	}

	device := o.startedDevice()
	if device == nil {
		return errors.New("no ublk device to flush")
	}

	return device.Sync(ctx)
}
