//go:build linux

package rootfs

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd/testutils"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/ublk"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// fakeUblkManager drives the provider's Start/Close without a real device:
// Open can be held open to race a Close, and both calls are counted.
type fakeUblkManager struct {
	entered chan struct{} // closed once Open is entered, when set
	release chan struct{} // Open waits for this, when set
	openErr error         // returned by Open
	device  *ublk.Device  // returned by Open when openErr is nil

	openCount  atomic.Int32
	closeCount atomic.Int32
}

func (m *fakeUblkManager) Open(context.Context, ublk.Backend, ublk.Options) (*ublk.Device, error) {
	m.openCount.Add(1)

	if m.entered != nil {
		close(m.entered)
	}

	if m.release != nil {
		<-m.release
	}

	return m.device, m.openErr
}

func (m *fakeUblkManager) Close() error {
	m.closeCount.Add(1)

	return nil
}

// newLifecycleTestProvider builds a provider around the fake manager with a
// real overlay over a temp cache, so Close can run its whole body.
func newLifecycleTestProvider(t *testing.T, manager ublkManager) *UblkProvider {
	t.Helper()

	const size = 8 << 20

	source, err := testutils.NewZeroDevice(size, header.RootfsBlockSize)
	require.NoError(t, err)

	cache, err := block.NewCache(size, header.RootfsBlockSize, filepath.Join(t.TempDir(), "rootfs.cow"), false)
	require.NoError(t, err)

	featureFlags, err := featureflags.NewClient()
	require.NoError(t, err)

	t.Cleanup(func() { _ = featureFlags.Close(t.Context()) })

	return &UblkProvider{
		overlay:            block.NewOverlay(source, cache),
		manager:            manager,
		ready:              utils.NewSetOnce[string](),
		finishedOperations: make(chan error, 1),
		featureFlags:       featureFlags,
	}
}

// TestUblkProviderStartRefusesAfterClose pins the lifecycle fix: a Start that
// loses the race against Close refuses instead of opening a device whose
// control plane is gone (such a device would never be closed, and the release
// signal has already gone out).
func TestUblkProviderStartRefusesAfterClose(t *testing.T) {
	t.Parallel()

	fake := &fakeUblkManager{openErr: errors.New("Open must not run after Close")}
	p := newLifecycleTestProvider(t, fake)

	require.NoError(t, p.Close(t.Context()))

	// SetOnce reports only double-sets; the refusal rides the ready promise.
	require.NoError(t, p.Start(t.Context()))
	_, waitErr := p.ready.Wait()
	require.ErrorContains(t, waitErr, "provider is closed")

	require.Zero(t, fake.openCount.Load(), "Start must not open a device after Close")
	require.Equal(t, int32(1), fake.closeCount.Load())
}

// TestUblkProviderCloseWaitsForInflightStart pins the serialization: Close
// waits for an in-flight Start's open to settle, so no device can be
// published after the teardown; the open's failure resolves the ready promise
// loudly.
func TestUblkProviderCloseWaitsForInflightStart(t *testing.T) {
	t.Parallel()

	openErr := errors.New("open failed for the test")
	fake := &fakeUblkManager{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		openErr: openErr,
	}
	p := newLifecycleTestProvider(t, fake)

	startDone := make(chan error, 1)
	go func() { startDone <- p.Start(t.Context()) }()

	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Open was not entered")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close(t.Context()) }()

	// While Open is held, Close must not have torn anything down.
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while Open was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.Zero(t, fake.closeCount.Load(), "Close must not tear down the control plane mid-Start")

	close(fake.release)

	require.NoError(t, <-startDone, "the open failure rides the ready promise, not the return")
	_, waitErr := p.ready.Wait()
	require.ErrorContains(t, waitErr, "open failed for the test")
	require.NoError(t, <-closeDone)
	require.Equal(t, int32(1), fake.closeCount.Load())
}

// TestUblkProviderStartIsIdempotent pins the selection's eager start: the
// first Start opens the device, and the sandbox wiring's later call is a
// no-op rather than a second device behind the first one's back.
func TestUblkProviderStartIsIdempotent(t *testing.T) {
	t.Parallel()

	fake := &fakeUblkManager{device: &ublk.Device{}}
	p := newLifecycleTestProvider(t, fake)

	require.NoError(t, p.Start(t.Context()))
	require.NoError(t, p.Start(t.Context()))

	_, waitErr := p.ready.Wait()
	require.NoError(t, waitErr)
	require.Equal(t, int32(1), fake.openCount.Load(), "a second Start must not open another device")
}
