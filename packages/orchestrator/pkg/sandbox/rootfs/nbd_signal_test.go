//go:build linux

package rootfs

import (
	"testing"
	"time"
)

// TestNBDProviderFinishedOperationsSignalNeverBlocks pins S-19: the release
// signal must not block on a full buffer (a second Close would hang teardown).
func TestNBDProviderFinishedOperationsSignalNeverBlocks(t *testing.T) {
	t.Parallel()

	p := &NBDProvider{finishedOperations: make(chan struct{}, 1)}

	done := make(chan struct{})
	go func() {
		defer close(done)

		p.signalFinishedOperations()
		p.signalFinishedOperations()
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
