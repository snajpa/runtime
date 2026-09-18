//go:build linux

package userfaultfd

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TestSourceReadDoesNotBlockRemove pins S-28: a MISSING-fault worker's source
// read runs OUTSIDE settleRequests, so the REMOVE batch (MADV_DONTNEED →
// tracker `removed`) must complete while the worker is parked in the read
// window. Before the fix the worker held the read lock across the read and
// its retries, and the batch queued behind it. After the release the fetched
// content must be dropped: a REMOVE'd page reads back as zeros, and the fault
// must still complete (page present, zero-filled).
func TestSourceReadDoesNotBlockRemove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pagesize uint64
	}{
		{name: "4k", pagesize: header.PageSize},
		{name: "hugepage", pagesize: header.HugepageSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			withRaceContext(t, func(ctx context.Context) {
				const sentinel = byte(0x5A)
				const pageIdx = 1
				pageOffset := int64(pageIdx) * int64(tt.pagesize)

				cfg := testConfig{
					pagesize:      tt.pagesize,
					numberOfPages: 4,
					barriers:      true,
					removeEnabled: true,
					sourcePatcher: func(content []byte) {
						content[pageOffset] = sentinel
					},
				}

				h, err := configureCrossProcessTest(ctx, t, cfg)
				require.NoError(t, err)

				memStart := uintptr(unsafe.Pointer(&(*h.memoryArea)[0]))
				addr := memStart + uintptr(pageIdx)*uintptr(tt.pagesize)

				token, err := h.installFaultBarrier(ctx, addr, faultPhaseBeforeSourceRead)
				require.NoError(t, err)

				readErrCh := make(chan error, 1)
				go func() {
					readErrCh <- h.executeRead(ctx, operation{offset: pageOffset, mode: operationModeRead})
				}()

				waitCtx, waitCancel := context.WithTimeout(ctx, barrierArrivalDeadline)
				err = h.waitFaultHeld(waitCtx, token)
				waitCancel()
				require.NoError(t, err, "worker for page %d (addr %#x) did not park at the source-read barrier", pageIdx, addr)

				// The worker holds no lock here: the batch must be PROCESSED
				// (tracker → Removed) while the worker is still parked.
				require.NoError(t, h.executeRemove(operation{offset: pageOffset, mode: operationModeRemove}))
				require.NoError(t, waitForState(ctx, h, uint64(pageOffset), block.Removed, madviseBudget),
					"REMOVE batch queued behind the parked worker: the source read still holds settleRequests")

				require.NoError(t, h.releaseFault(ctx, token))

				select {
				case <-readErrCh:
				case <-ctx.Done():
					t.Fatalf("read of page %d did not unblock after barrier release", pageIdx)
				}

				page := (*h.memoryArea)[pageOffset : pageOffset+int64(tt.pagesize)]

				// MADV_POPULATE_READ resolves the page via the kernel fault
				// path while this goroutine sits in _Gsyscall, so the direct
				// load below can never fault.
				err = unix.Madvise(page, unix.MADV_POPULATE_READ)
				require.NoError(t, err, "madvise POPULATE_READ at offset %d", pageOffset)
				assert.Equal(t, byte(0), page[0],
					"page %d first byte: want 0 (the REMOVE won while the read was outside the lock); got %#x — "+
						"the stale source content (sentinel %#x) must be dropped, not installed",
					pageIdx, page[0], sentinel,
				)

				// The dropped read leaves an installed zero page behind (the
				// REMOVE'd page was re-installed as zeros).
				require.NoError(t, waitForState(ctx, h, uint64(pageOffset), block.Zero, barrierArrivalDeadline),
					"the dropped source read must leave an installed zero page behind")

				pagemap, err := testutils.NewPagemapReader()
				require.NoError(t, err)
				defer pagemap.Close()

				entry, err := pagemap.ReadEntry(addr)
				require.NoError(t, err)
				assert.True(t, entry.IsPresent(), "page %d should be present after the racing read", pageIdx)
			})
		})
	}
}

// flakyReader fails a fixed number of reads, then serves the page.
type flakyReader struct {
	page     []byte
	failures atomic.Int32
	reads    atomic.Int32
}

func (r *flakyReader) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	r.reads.Add(1)

	if r.failures.Add(-1) >= 0 {
		return 0, errors.New("source unavailable")
	}

	return copy(p, r.page), nil
}

func newReadSourcePageUffd(src PageReader) *Userfaultfd {
	return &Userfaultfd{
		pageSize: header.PageSize,
		src:      src,
		logger:   logger.NewNopLogger(),
	}
}

// TestReadSourcePageRetriesBounded: the retry policy survives the S-28 move
// out of the lock — two transient failures then success still deliver the
// page.
func TestReadSourcePageRetriesBounded(t *testing.T) {
	t.Parallel()

	src := &flakyReader{page: bytes.Repeat([]byte{0xAB}, int(header.PageSize))}
	src.failures.Store(2)

	u := newReadSourcePageUffd(src)

	data, release, err := u.readSourcePage(t.Context(), 0, nil)
	defer release()
	require.NoError(t, err)
	assert.Equal(t, int32(3), src.reads.Load(), "two failures plus the successful attempt")
	require.Len(t, data, int(header.PageSize))
	assert.Equal(t, byte(0xAB), data[0])
}

// TestReadSourcePageFailureEscalates: the retries stay bounded, the
// escalation (serve-exit signal) fires exactly once, and the failure is
// returned for the worker to record.
func TestReadSourcePageFailureEscalates(t *testing.T) {
	t.Parallel()

	src := &flakyReader{page: bytes.Repeat([]byte{0xAB}, int(header.PageSize))}
	src.failures.Store(int32(sliceMaxRetries + 1))

	u := newReadSourcePageUffd(src)

	var escalations atomic.Int32

	_, release, err := u.readSourcePage(t.Context(), 0, func() error {
		escalations.Add(1)

		return nil
	})
	defer release()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read from source")
	assert.Equal(t, int32(1), escalations.Load())
	assert.Equal(t, int32(sliceMaxRetries+1), src.reads.Load())
}

// TestReadSourcePageHonorsCancelDuringBackoff: a cancelled context breaks out
// of the backoff instead of waiting out the delays.
func TestReadSourcePageHonorsCancelDuringBackoff(t *testing.T) {
	t.Parallel()

	src := &flakyReader{page: bytes.Repeat([]byte{0xAB}, int(header.PageSize))}
	src.failures.Store(100)

	u := newReadSourcePageUffd(src)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, release, err := u.readSourcePage(ctx, 0, nil)
	defer release()
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second, "a cancelled backoff must not wait out the delays")
}
