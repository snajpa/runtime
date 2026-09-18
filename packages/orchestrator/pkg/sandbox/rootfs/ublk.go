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

	finishedOperations chan struct{}

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
		finishedOperations: make(chan struct{}, 1),
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

	select {
	case <-o.finishedOperations:
	case <-ctx.Done():
		// Close the cache to avoid leaking the mmaped memory. Log an error
		// if that failed
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, errors.New("timeout waiting for overlay device to be released")
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

	// A provider that never started a device has nothing to flush; syncing
	// anyway would report a spurious "no ublk device to flush" on error paths.
	if o.startedDevice() != nil {
		err = o.sync(ctx)
		if err != nil {
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
				errs = append(errs, fmt.Errorf("error closing ublk device: %w", err))
			}
		}

		if err := o.manager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing ublk control device: %w", err))
		}

		// The device is gone, so nothing can reach the cache any more; this is
		// the point the export paths wait for before they read it.
		o.finishedOperations <- struct{}{}
	}

	err = o.overlay.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing overlay cache: %w", err))
	}

	logger.L().Info(ctx, "overlay device released")

	return errors.Join(errs...)
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
