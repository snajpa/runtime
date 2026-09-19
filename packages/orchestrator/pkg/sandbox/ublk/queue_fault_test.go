//go:build linux

package ublk

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
)

type faultProbeBase struct{ block.ReadonlyDevice }

func (faultProbeBase) BlockSize() int64 { return 4096 }

// TestQueueOwnerContainsMappedBackendFault pins the review's P1: a backing
// file I/O fault on the mmap-backed backend must be contained as an error
// (EIO to the kernel) instead of escaping the queue owner goroutine; the
// probe mirrors reviewer0's truncated-mapping case.
func TestQueueOwnerContainsMappedBackendFault(t *testing.T) {
	t.Parallel()

	const bs, size = 4096, 2 * 4096

	path := filepath.Join(t.TempDir(), "fault.cow")

	c, err := block.NewCache(size, bs, path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	o := &queueOwner{dev: &Device{backend: block.NewOverlay(faultProbeBase{}, c), ctx: t.Context()}}

	page := make([]byte, bs)
	if _, err := o.backendWriteAt(page, 0); err != nil {
		t.Fatalf("warm write: %v", err)
	}

	// Mapped pages beyond EOF raise SIGBUS on access, like a bad sector.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := o.backendReadAt(page, 0); err == nil {
		t.Fatal("a mapped backend fault must be contained and returned as an error")
	}
}
