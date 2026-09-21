//go:build linux

package ublk

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestCloseWithInFlightIO closes a device while a second opener keeps issuing
// direct reads, so requests are still arriving on the queue when the stop runs.
// The kernel waits for the requests it already dispatched before it lets a
// device stop, so a queue task that walks away from one leaves STOP_DEV inside
// del_gendisk forever: this is the regression test for that teardown ordering.
func TestCloseWithInFlightIO(t *testing.T) {
	t.Parallel()

	mgr := requireUblk(t)
	ctx := t.Context()

	backend := &fakeBackend{data: make([]byte, 16<<20)}

	dev, err := mgr.Open(ctx, backend, DefaultOptions())
	require.NoError(t, err)

	node := dev.Path()

	// Direct reads bypass the cache, so every read is a real request on the
	// queue rather than a page-cache hit.
	reader, err := unix.Open(node, unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(reader) }()

	buf, err := unix.Mmap(-1, 0, 128<<10, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_ANON)
	require.NoError(t, err)
	defer func() { _ = unix.Munmap(buf) }()

	stop := make(chan struct{})
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)

		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}

			// A device that is gone fails the read, which also ends the loop.
			if _, err := unix.Pread(reader, buf, int64(i%128)*(128<<10)); err != nil {
				return
			}

			// Keep the queue idle between reads: the barrier before the stop
			// has to be able to observe an idle queue, while the reads still
			// land close enough to the stop to be dispatched by it.
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// Let the reader get going before the close.
	time.Sleep(100 * time.Millisecond)

	closed := make(chan error, 1)

	go func() { closed <- dev.Close(ctx) }()

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("Close did not finish while I/O was in flight")
	}

	close(stop)
	<-readerDone

	_, err = os.Stat(node)
	require.True(t, os.IsNotExist(err), "device node %s is still present after close", node)
}
