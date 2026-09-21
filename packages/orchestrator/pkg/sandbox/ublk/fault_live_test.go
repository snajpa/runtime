//go:build linux

package ublk

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// faultBackend drives real queue requests into two teardown-sensitive backend
// behaviors. The tests are intentionally opt-in: they create owned kernel
// devices and must never mutate the host during ordinary package tests.
type faultBackend struct {
	data []byte

	blockRead  bool
	blockWrite bool
	readArmed  atomic.Bool
	writeArmed atomic.Bool

	readStarted  chan struct{}
	writeStarted chan struct{}
	releaseRead  chan struct{}
	releaseWrite chan struct{}

	readStartedOnce  sync.Once
	writeStartedOnce sync.Once
	releaseReadOnce  sync.Once
	releaseWriteOnce sync.Once
}

func newFaultBackend(blockRead, blockWrite bool) *faultBackend {
	return &faultBackend{
		data:         make([]byte, 16<<20),
		blockRead:    blockRead,
		blockWrite:   blockWrite,
		readStarted:  make(chan struct{}),
		writeStarted: make(chan struct{}),
		releaseRead:  make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (b *faultBackend) Size(context.Context) (int64, error) { return int64(len(b.data)), nil }
func (b *faultBackend) BlockSize() int64                    { return 4096 }

func (b *faultBackend) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if b.blockRead && b.readArmed.Load() {
		b.readStartedOnce.Do(func() { close(b.readStarted) })
		select {
		case <-b.releaseRead:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if off < 0 || off+int64(len(p)) > int64(len(b.data)) {
		return 0, unix.EINVAL
	}

	return copy(p, b.data[off:]), nil
}

func (b *faultBackend) WriteAt(p []byte, off int64) (int, error) {
	if b.blockWrite && b.writeArmed.Load() {
		b.writeStartedOnce.Do(func() { close(b.writeStarted) })
		<-b.releaseWrite
	}
	if off < 0 || off+int64(len(p)) > int64(len(b.data)) {
		return 0, unix.EINVAL
	}
	copy(b.data[off:], p)

	return len(p), nil
}

func (b *faultBackend) WriteZeroesAt(off, length int64) (int, error) {
	if off < 0 || off+length > int64(len(b.data)) {
		return 0, unix.EINVAL
	}
	for i := off; i < off+length; i++ {
		b.data[i] = 0
	}

	return int(length), nil
}

func (b *faultBackend) armRead()  { b.readArmed.Store(true) }
func (b *faultBackend) armWrite() { b.writeArmed.Store(true) }

func (b *faultBackend) unblockRead() {
	b.releaseReadOnce.Do(func() { close(b.releaseRead) })
}

func (b *faultBackend) unblockWrite() {
	b.releaseWriteOnce.Do(func() { close(b.releaseWrite) })
}

func openLiveDevice(t *testing.T, backend Backend) (*Manager, *Device, error) {
	t.Helper()

	mgr := requireUblk(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	dev, err := mgr.Open(ctx, backend, DefaultOptions())

	return mgr, dev, err
}

func alignedDirectBuffer(t *testing.T, size int) []byte {
	t.Helper()

	buf, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_ANON)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Munmap(buf) })

	return buf
}

func waitReleaseProven(t *testing.T, dev *Device) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		switch state := dev.ReleaseState(); state {
		case ReleaseProven:
			return
		case ReleaseQuarantined:
			t.Fatalf("device became quarantined before release: %v", dev.Failure())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("device did not reach ReleaseProven; state=%d failure=%v", dev.ReleaseState(), dev.Failure())
}

func TestAbortWithCooperativeBlockedReadReleases(t *testing.T) {
	backend := newFaultBackend(true, false)
	_, dev, openErr := openLiveDevice(t, backend)
	if dev != nil {
		t.Cleanup(func() {
			backend.unblockRead()
			_ = dev.Abort(context.Background(), errors.New("live read cleanup"))
		})
	}
	require.NoError(t, openErr)
	require.NotNil(t, dev)

	reader, err := unix.Open(dev.Path(), unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(reader) })
	buf := alignedDirectBuffer(t, 4096)
	backend.armRead()

	readDone := make(chan error, 1)
	go func() {
		_, err := unix.Pread(reader, buf, 0)
		readDone <- err
	}()
	select {
	case <-backend.readStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("backend ReadAt was not entered")
	}

	cause := errors.New("cooperative read abort")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.ErrorIs(t, dev.Abort(ctx, cause), cause)
	waitReleaseProven(t, dev)

	select {
	case err := <-readDone:
		require.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("blocked read did not complete after cooperative abort")
	}
}

func TestAbortWithNonContextBlockedWriteReleasesLate(t *testing.T) {
	backend := newFaultBackend(false, true)
	_, dev, openErr := openLiveDevice(t, backend)
	if dev != nil {
		t.Cleanup(func() {
			backend.unblockWrite()
			_ = dev.Abort(context.Background(), errors.New("live write cleanup"))
		})
	}
	require.NoError(t, openErr)
	require.NotNil(t, dev)

	writer, err := unix.Open(dev.Path(), unix.O_WRONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(writer) })
	buf := alignedDirectBuffer(t, 4096)
	backend.armWrite()
	buf[0] = 0xa5

	writeDone := make(chan error, 1)
	go func() {
		_, err := unix.Pwrite(writer, buf, 0)
		writeDone <- err
	}()
	select {
	case <-backend.writeStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("backend WriteAt was not entered")
	}

	cause := errors.New("non-context write abort")
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	abortErr := dev.Abort(ctx, cause)
	require.Error(t, abortErr)
	require.Equal(t, ReleasePending, dev.ReleaseState())

	backend.unblockWrite()
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("blocked write did not complete after release")
	}

	waitReleaseProven(t, dev)
	require.Error(t, dev.Close(context.Background()), "caller timeout must remain sticky after late release")
	require.Equal(t, ReleaseProven, dev.ReleaseState())
}
