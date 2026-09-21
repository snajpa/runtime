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
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/ublk"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// Transport identifies the transport actually serving a provider.
type Transport string

const (
	TransportNBD    Transport = "nbd"
	TransportUblk   Transport = "ublk"
	TransportDirect Transport = "direct"
)

// UblkProvider serves the existing Overlay through one low-level ublk Device.
// It owns the writable and sealing caches, while the immutable template device
// remains owned by the template/rootfs caller as it is for NBD.
type UblkProvider struct {
	overlay *block.Overlay
	manager ublkManager
	device  ublkDevice
	ready   *utils.SetOnce[string]

	cachePath string
	sealGen   atomic.Int64

	startOnce        sync.Once
	startErr         error
	closeOnce        sync.Once
	closeErr         error
	finishOnce       sync.Once
	finished         chan struct{}
	closeReadyOnce   sync.Once
	closeResultReady chan struct{}
	releaseMu        sync.Mutex
	releaseErr       error
	ejectedMu        sync.Mutex
	ejected          *block.Cache
	abandoned        bool
}

var _ Provider = (*UblkProvider)(nil)

type ublkDevice interface {
	Path() string
	Failure() error
	Sync(context.Context) error
	Close(context.Context) error
	Abort(context.Context, error) error
	ReleaseState() ublk.ReleaseState
}

type ublkManager interface {
	Open(context.Context, ublk.Backend, ublk.Options) (ublkDevice, error)
	Close() error
}

type managerAdapter struct{ manager *ublk.Manager }

func (m managerAdapter) Open(ctx context.Context, backend ublk.Backend, opts ublk.Options) (ublkDevice, error) {
	device, err := m.manager.Open(ctx, backend, opts)
	if device == nil {
		return nil, err
	}
	return device, err
}

func (m managerAdapter) Close() error { return m.manager.Close() }

// NewUblkProvider constructs an unstarted provider. Start must complete before
// Firecracker is attached because fallback is only safe before that boundary.
func NewUblkProvider(ctx context.Context, rootfs block.ReadonlyDevice, cachePath string) (*UblkProvider, error) {
	size, err := rootfs.Size(ctx)
	if err != nil {
		return nil, &ublk.OpenError{Kind: ublk.OpenFailureNoSideEffect, Err: fmt.Errorf("error getting device size: %w", err)}
	}

	manager, err := ublk.NewManager()
	if err != nil {
		return nil, &ublk.OpenError{Kind: ublk.OpenFailureNoSideEffect, Err: fmt.Errorf("error opening ublk control: %w", err)}
	}

	cache, err := block.NewCache(size, rootfs.BlockSize(), cachePath, false)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("error creating cache: %w", err), manager.Close())
	}

	return &UblkProvider{
		overlay:          block.NewOverlay(rootfs, cache),
		manager:          managerAdapter{manager: manager},
		cachePath:        cachePath,
		ready:            utils.NewSetOnce[string](),
		finished:         make(chan struct{}),
		closeResultReady: make(chan struct{}),
	}, nil
}

// NewUblkProviderWithFallback starts ublk synchronously. It falls back to NBD
// only when low-level admission proves that no ublk side effect remains.
func NewUblkProviderWithFallback(
	ctx context.Context,
	rootfs block.ReadonlyDevice,
	cachePath string,
	devicePool *nbd.DevicePool,
	featureFlags *featureflags.Client,
) (Provider, Transport, error) {
	provider, err := NewUblkProvider(ctx, rootfs, cachePath)
	if err != nil {
		var openErr *ublk.OpenError
		if !errors.As(err, &openErr) || !openErr.FallbackSafe() {
			return nil, "", err
		}
		return newStartedNBDProvider(ctx, rootfs, cachePath, devicePool, featureFlags, err)
	}

	if err = provider.Start(ctx); err == nil {
		return provider, TransportUblk, nil
	}

	var openErr *ublk.OpenError
	if !errors.As(err, &openErr) || !openErr.FallbackSafe() {
		// Retain the provider/device ownership on partial side effects. The
		// terminal worker owns any eventual cleanup; fallback is forbidden.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = provider.Close(cleanupCtx)
		cancel()
		return provider, "", fmt.Errorf("ublk rootfs admission failed: %w", err)
	}

	// No device side effect was observed. Release the failed ublk cache/control
	// ownership before constructing the NBD replacement. Same-path reuse is
	// forbidden if either local close fails.
	if err := provider.closeBeforeFallback(); err != nil {
		return nil, "", errors.Join(err, fmt.Errorf("ublk fallback cleanup failed: %w", err))
	}
	return newStartedNBDProvider(ctx, rootfs, cachePath, devicePool, featureFlags, err)
}

func newStartedNBDProvider(
	ctx context.Context,
	rootfs block.ReadonlyDevice,
	cachePath string,
	devicePool *nbd.DevicePool,
	featureFlags *featureflags.Client,
	cause error,
) (Provider, Transport, error) {
	provider, err := NewNBDProvider(ctx, rootfs, cachePath, devicePool, featureFlags)
	if err != nil {
		return nil, "", errors.Join(cause, err)
	}
	if err = provider.Start(ctx); err != nil {
		_ = provider.Close(context.Background())
		return nil, "", errors.Join(cause, err)
	}
	return provider, TransportNBD, nil
}

func (p *UblkProvider) Start(ctx context.Context) error {
	p.startOnce.Do(func() {
		opts := ublk.DefaultOptions()
		opts.BlockSize = p.overlay.BlockSize()
		device, err := p.manager.Open(ctx, p.overlay, opts)
		p.device = device
		if err != nil {
			p.startErr = err
			_ = p.ready.SetError(err)
			return
		}
		if device == nil {
			p.startErr = errors.New("ublk manager returned no device")
			_ = p.ready.SetError(p.startErr)
			return
		}
		p.startErr = p.ready.SetValue(device.Path())
	})
	return p.startErr
}

func (p *UblkProvider) Path() (string, error) { return p.ready.Wait() }

func (p *UblkProvider) sync(ctx context.Context) error {
	if p.device == nil {
		if p.startErr != nil {
			return p.startErr
		}
		return errors.New("ublk provider has not started")
	}
	if err := p.device.Failure(); err != nil {
		return err
	}
	return p.device.Sync(ctx)
}

func (p *UblkProvider) ExportDiffInPlace(ctx context.Context, out *os.File) (*header.DiffMetadata, error) {
	if err := p.sync(ctx); err != nil {
		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}
	return p.overlay.ExportDiffInPlace(ctx, out)
}

func (p *UblkProvider) ExportDiff(ctx context.Context, out *os.File, closeSandbox func(context.Context) error) (*header.DiffMetadata, error) {
	cache, err := p.ejectAndStopSandbox(ctx, closeSandbox)
	if err != nil {
		return nil, err
	}

	metadata, err := cache.ExportToDiff(ctx, out)
	closeErr := cache.Close()
	p.ejectedMu.Lock()
	if closeErr == nil {
		if p.ejected == cache {
			p.ejected = nil
		}
	} else {
		p.abandoned = true
	}
	p.ejectedMu.Unlock()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return metadata, nil
}

func (p *UblkProvider) PrepareExportDiff(ctx context.Context, closeSandbox func(context.Context) error) (*block.Cache, error) {
	cache, err := p.ejectAndStopSandbox(ctx, closeSandbox)
	if err != nil {
		return nil, err
	}
	if err := p.handoffEjectedCache(cache); err != nil {
		return nil, errors.Join(err, cache.Close())
	}
	return cache, nil
}

func (p *UblkProvider) SwapForBackgroundSeal(ctx context.Context) (*block.Cache, error) {
	if err := p.sync(ctx); err != nil {
		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}

	size, err := p.overlay.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting overlay size: %w", err)
	}

	gen := p.sealGen.Add(1)
	// Keep the .cow suffix so startup reclaim finds fresh live caches. The
	// previous cache is returned frozen in the overlay sealing slot.
	freshPath := fmt.Sprintf("%s-seal%d.cow", strings.TrimSuffix(p.cachePath, ".cow"), gen)
	fresh, err := block.NewCache(size, p.overlay.BlockSize(), freshPath, false)
	if err != nil {
		return nil, fmt.Errorf("creating fresh cache: %w", err)
	}

	old, err := p.overlay.SwapCache(fresh)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("swapping cache: %w", err), fresh.Close())
	}
	return old, nil
}

func (p *UblkProvider) FoldSealed(context.Context) (*block.Cache, error) {
	return p.overlay.FoldSealing()
}

func (p *UblkProvider) ejectAndStopSandbox(
	ctx context.Context,
	closeSandbox func(context.Context) error,
) (*block.Cache, error) {
	cache, err := p.overlay.EjectCache()
	if err != nil {
		return nil, fmt.Errorf("error ejecting cache: %w", err)
	}
	p.ejectedMu.Lock()
	p.ejected = cache
	p.ejectedMu.Unlock()

	go func() { _ = closeSandbox(ctx) }()

	select {
	case <-p.finished:
		if err := p.closeResult(); err != nil {
			return nil, errors.Join(fmt.Errorf("ublk cleanup failed: %w", err), p.abandonEjectedCache(true))
		}
		return cache, nil
	case <-ctx.Done():
		allowClose := p.device == nil || p.device.ReleaseState() == ublk.ReleaseProven
		return nil, errors.Join(fmt.Errorf("timeout waiting for ublk device release: %w", ctx.Err()), p.abandonEjectedCache(allowClose))
	}
}

func (p *UblkProvider) handoffEjectedCache(cache *block.Cache) error {
	p.ejectedMu.Lock()
	defer p.ejectedMu.Unlock()
	if p.ejected != cache {
		return errors.New("ejected cache ownership changed before handoff")
	}
	p.ejected = nil
	return nil
}

func (p *UblkProvider) Close(ctx context.Context) error {
	p.closeOnce.Do(func() {
		if p.device == nil {
			p.storeCloseErr(errors.Join(p.startErr, p.overlay.Close(), p.manager.Close()))
			p.markCloseResultReady()
			p.signalFinished()
			return
		}

		var terminalErr error
		if p.startErr != nil {
			terminalErr = p.device.Abort(ctx, p.startErr)
		} else if syncErr := p.sync(ctx); syncErr != nil {
			terminalErr = p.device.Abort(ctx, syncErr)
		} else {
			terminalErr = p.device.Close(ctx)
		}
		releaseErr, released := p.finishWhenReleased()
		p.storeCloseErr(errors.Join(terminalErr, releaseErr))
		p.markCloseResultReady()
		if released {
			p.signalFinished()
		}
	})
	return p.closeResult()
}

func (p *UblkProvider) closeResult() error {
	p.releaseMu.Lock()
	err := errors.Join(p.closeErr, p.releaseErr)
	p.releaseMu.Unlock()
	if p.device != nil {
		err = errors.Join(err, p.device.Failure())
	}
	return err
}

func (p *UblkProvider) storeCloseErr(err error) {
	p.releaseMu.Lock()
	p.closeErr = err
	p.releaseMu.Unlock()
}

func (p *UblkProvider) markCloseResultReady() {
	if p.closeResultReady != nil {
		p.closeReadyOnce.Do(func() { close(p.closeResultReady) })
	}
}

func (p *UblkProvider) waitCloseResultReady() {
	if p.closeResultReady != nil {
		<-p.closeResultReady
	}
}

func (p *UblkProvider) closeBeforeFallback() error {
	p.closeOnce.Do(func() {
		p.storeCloseErr(errors.Join(p.overlay.Close(), p.manager.Close()))
		p.markCloseResultReady()
		p.signalFinished()
	})
	return p.closeResult()
}

func (p *UblkProvider) finishWhenReleased() (error, bool) {
	if p.device.ReleaseState() == ublk.ReleaseProven {
		return p.release(), true
	}
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			switch p.device.ReleaseState() {
			case ublk.ReleaseProven:
				_ = p.release()
				p.waitCloseResultReady()
				p.signalFinished()
				return
			case ublk.ReleaseQuarantined:
				return
			}
		}
	}()
	return nil, false
}

func (p *UblkProvider) release() error {
	err := errors.Join(p.overlay.Close(), p.manager.Close())
	p.ejectedMu.Lock()
	ejected := p.ejected
	if p.abandoned {
		p.ejected = nil
	} else {
		ejected = nil
	}
	p.ejectedMu.Unlock()
	if ejected != nil {
		if closeErr := ejected.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
			p.ejectedMu.Lock()
			if p.ejected == nil {
				p.ejected = ejected
			}
			p.ejectedMu.Unlock()
		}
	}
	p.releaseMu.Lock()
	p.releaseErr = errors.Join(p.releaseErr, err)
	p.releaseMu.Unlock()
	return err
}

func (p *UblkProvider) cleanupError() error {
	p.releaseMu.Lock()
	defer p.releaseMu.Unlock()

	return p.releaseErr
}

func (p *UblkProvider) abandonEjectedCache(allowClose bool) error {
	p.ejectedMu.Lock()
	p.abandoned = true
	var cache *block.Cache
	if allowClose {
		cache = p.ejected
		p.ejected = nil
	}
	p.ejectedMu.Unlock()
	if cache == nil {
		return nil
	}
	if err := cache.Close(); err != nil {
		p.ejectedMu.Lock()
		if p.ejected == nil {
			p.ejected = cache
		}
		p.ejectedMu.Unlock()
		return err
	}
	return nil
}

func (p *UblkProvider) signalFinished() {
	p.finishOnce.Do(func() { close(p.finished) })
}
