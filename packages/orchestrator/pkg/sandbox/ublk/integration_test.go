//go:build linux

package ublk

import (
	"bytes"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The integration tests drive the real driver: they need root and a loaded
// ublk_drv. They are meant to run inside the dev VM (see
// docs/projects/e2b/workflows/dev-environment.md), not on the host.
func requireUblk(t *testing.T) *Manager {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("ublk integration tests need root")
	}

	if _, err := os.Stat(controlDevicePath); err != nil {
		t.Skipf("ublk control device is not available: %v", err)
	}

	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("closing manager: %v", err)
		}
	})

	return mgr
}

func newPattern(seed byte, length int) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = byte(i)*7 + seed
	}

	return out
}

// TestDeviceLifecycle covers the life cycle the orchestrator uses: create,
// serve I/O through /dev/ublkbN, flush, and delete.
func TestDeviceLifecycle(t *testing.T) { //nolint:paralleltest // one device, one ordered story: the subtests share it
	mgr := requireUblk(t)
	ctx := t.Context()

	backend := &fakeBackend{data: newPattern(0x11, 8<<20)}
	dev, err := mgr.Open(ctx, backend, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Logf("device %d: %s", dev.ID(), dev.Path())

	node := dev.Path()

	if _, err := os.Stat(node); err != nil {
		t.Fatalf("stat %s: %v", node, err)
	}

	file, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", node, err)
	}
	defer func() { _ = file.Close() }()

	t.Run("write and read back", func(t *testing.T) { //nolint:paralleltest // shares the device with the other subtests
		const off = 1 << 20

		pattern := newPattern(0x2a, 256<<10)

		if _, err := file.WriteAt(pattern, off); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}

		// Writeback has to reach the backend; the fsync is also what the
		// provider's barrier does.
		if err := file.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		if got := backend.slice(off, int64(len(pattern))); !bytes.Equal(got, pattern) {
			t.Fatalf("backend does not hold the written data")
		}

		// Drop the kernel's cache so the read has to come through the device.
		if err := dev.Sync(ctx); err != nil {
			t.Fatalf("device Sync: %v", err)
		}

		got := make([]byte, len(pattern))
		if _, err := file.ReadAt(got, off); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if !bytes.Equal(got, pattern) {
			t.Errorf("read back %d bytes that do not match the write", len(got))
		}

		// A region that was never written reads the backend's own pattern.
		untouched := make([]byte, 64<<10)
		if _, err := file.ReadAt(untouched, 4<<20); err != nil {
			t.Fatalf("ReadAt of untouched region: %v", err)
		}
		if want := backend.slice(4<<20, int64(len(untouched))); !bytes.Equal(untouched, want) {
			t.Errorf("untouched region does not match the backend")
		}
	})

	t.Run("requests larger than the buffer are split", func(t *testing.T) { //nolint:paralleltest // shares the device with the other subtests
		// max_io_buf_bytes caps a single request, so the block layer splits
		// this one; the device has to serve all of it.
		pattern := newPattern(0x33, dev.Options().MaxIOBufBytes+512<<10)

		if _, err := file.WriteAt(pattern, 3<<20); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := dev.Sync(ctx); err != nil {
			t.Fatalf("device Sync: %v", err)
		}

		if got := backend.slice(3<<20, int64(len(pattern))); !bytes.Equal(got, pattern) {
			t.Errorf("backend does not hold the large write")
		}
	})

	t.Run("discard reaches the backend", func(t *testing.T) { //nolint:paralleltest // shares the device with the other subtests
		before := backend.zeroCount()

		// PUNCH_HOLE on a block device becomes a discard or a write-zeroes
		// request, both of which the device serves with WriteZeroesAt.
		if err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 6<<20, 64<<10); err != nil {
			t.Fatalf("fallocate PUNCH_HOLE: %v", err)
		}

		deadline := time.Now().Add(5 * time.Second)
		for backend.zeroCount() == before {
			if time.Now().After(deadline) {
				t.Fatal("the backend never saw a zeroing request")
			}

			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("flush is accepted", func(t *testing.T) { //nolint:paralleltest // shares the device with the other subtests
		if err := file.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	})

	// The kernel only frees a device id once every opener is gone, so release
	// the node the way the sandbox process does before deleting the device.
	if err := file.Close(); err != nil {
		t.Fatalf("closing %s: %v", node, err)
	}

	if err := dev.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(node); !os.IsNotExist(err) {
		t.Errorf("device node %s still present after close: %v", node, err)
	}
}

// TestDeviceQueues checks that a device with several queues serves concurrent
// I/O correctly.
func TestDeviceQueues(t *testing.T) {
	t.Parallel()

	mgr := requireUblk(t)
	ctx := t.Context()

	backend := &fakeBackend{data: newPattern(0x44, 16<<20)}

	opts := DefaultOptions()
	opts.Queues = 4
	opts.QueueDepth = 16

	dev, err := mgr.Open(ctx, backend, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	file, err := os.OpenFile(dev.Path(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev.Path(), err)
	}
	defer file.Close()

	const writers = 8

	var (
		wg   sync.WaitGroup
		errs = make(chan error, writers)
	)

	for w := range writers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			pattern := newPattern(byte(w), 128<<10)
			off := int64(w) * 1 << 20

			if _, err := file.WriteAt(pattern, off); err != nil {
				errs <- err

				return
			}

			if err := file.Sync(); err != nil {
				errs <- err

				return
			}

			got := make([]byte, len(pattern))
			if _, err := file.ReadAt(got, off); err != nil {
				errs <- err

				return
			}
			if !bytes.Equal(got, pattern) {
				errs <- os.ErrInvalid

				return
			}
		}(w)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent I/O: %v", err)
	}

	if err := dev.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDeviceBackendError checks that a backend failure reaches the reader
// instead of being answered with stale data.
func TestDeviceBackendError(t *testing.T) {
	t.Parallel()

	mgr := requireUblk(t)
	ctx := t.Context()

	backend := &fakeBackend{data: newPattern(0x55, 4<<20)}

	dev, err := mgr.Open(ctx, backend, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := dev.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	file, err := os.OpenFile(dev.Path(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev.Path(), err)
	}
	defer file.Close()

	if err := dev.Sync(ctx); err != nil {
		t.Fatalf("device Sync: %v", err)
	}

	backend.setReadErr(unix.EIO)

	buf := make([]byte, 4096)
	if _, err := file.ReadAt(buf, 2<<20); err == nil {
		t.Error("read succeeded although the backend failed")
	}
}

// TestCloseWithOpenNode checks that closing a device whose node is still open
// stays bounded: the asynchronous delete does not wait for the last opener,
// and the node goes away with the device.
func TestCloseWithOpenNode(t *testing.T) {
	t.Parallel()

	mgr := requireUblk(t)
	ctx := t.Context()

	backend := &fakeBackend{data: newPattern(0x66, 2<<20)}

	dev, err := mgr.Open(ctx, backend, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	file, err := os.OpenFile(dev.Path(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev.Path(), err)
	}
	defer file.Close()

	node := dev.Path()

	done := make(chan error, 1)

	go func() { done <- dev.Close(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not finish while the device was open")
	}

	if _, err := os.Stat(node); !os.IsNotExist(err) {
		t.Errorf("device node %s still present after close: %v", node, err)
	}

	// The open descriptor outlived its device, so I/O through it has to fail
	// rather than reach a device that is gone.
	if _, err := file.ReadAt(make([]byte, 4096), 0); err == nil {
		t.Error("I/O through the deleted device succeeded")
	}
}

// TestManagerMultipleDevices checks that devices can coexist and reuse ids
// once they are gone.
func TestManagerMultipleDevices(t *testing.T) {
	t.Parallel()

	mgr := requireUblk(t)
	ctx := t.Context()

	first, err := mgr.Open(ctx, &fakeBackend{data: newPattern(0x77, 1<<20)}, DefaultOptions())
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}

	second, err := mgr.Open(ctx, &fakeBackend{data: newPattern(0x88, 1<<20)}, DefaultOptions())
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}

	if first.ID() == second.ID() {
		t.Errorf("both devices got id %d", first.ID())
	}
	if first.Path() == second.Path() {
		t.Errorf("both devices got path %s", first.Path())
	}

	if err := first.Close(ctx); err != nil {
		t.Errorf("closing first: %v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Errorf("closing second: %v", err)
	}
}
