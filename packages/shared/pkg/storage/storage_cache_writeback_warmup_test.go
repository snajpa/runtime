package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestWritebackDropsOnAWarmup measures what a cold warm-up costs the bounded
// writeback queue (S-12; review follow-up P-2): a warm-up that stays under the
// queue's concurrency warms the cache completely, and one that meets a
// saturated queue (a noisy tenant holding the slots) has its fills dropped —
// which costs one refetch on the next read, never data, because the read that
// triggered the fill was already served.
//
// The queue is process-wide (writebacks), so this test runs without t.Parallel.
//
//nolint:paralleltest // measures the process-wide writeback queue, so it must not overlap other tests
func TestWritebackDropsOnAWarmup(t *testing.T) {
	const (
		chunkSize   = 4096
		chunks      = 256
		fillLatency = 20 * time.Millisecond
	)

	// warm reads `chunks` distinct cold chunks in batches of at most `fanOut`
	// readers in flight, waits for each batch's admitted fills before starting
	// the next, and reports how many landed as cache files (the fills the queue
	// dropped leave none). Draining between batches keeps this cache at or below
	// `fanOut` of the process-wide queue's slots, so a warm-up that stays under
	// the queue's concurrency cannot drop fills by outrunning the drain.
	warm := func(t *testing.T, fanOut int) (landed int) {
		t.Helper()

		ctx := t.Context()
		cacheDir := t.TempDir()

		inner := NewMockSeekable(t)
		inner.EXPECT().OpenRangeReader(mock.Anything, mock.Anything, mock.Anything, (*FrameTable)(nil)).
			RunAndReturn(func(_ context.Context, off, length int64, _ *FrameTable) (RangeReader, Source, error) {
				// A cold fetch is not instant, so a warm-up with many readers in
				// flight completes its fills together.
				time.Sleep(fillLatency)

				return bytesRangeReader(bytes.Repeat([]byte{byte(off / chunkSize)}, int(length))), SourceFS, nil
			}).Maybe()

		c := &cachedSeekable{path: cacheDir, chunkSize: chunkSize, inner: inner, tracer: noopTracer}

		var (
			served   atomic.Int64
			failures atomic.Int64
		)

		for first := 0; first < chunks; first += fanOut {
			count := min(fanOut, chunks-first)

			var batch sync.WaitGroup

			for index := range count {
				batch.Go(func() {
					off := int64(first+index) * chunkSize

					rc, _, err := c.OpenRangeReader(ctx, off, chunkSize, nil)
					if err != nil {
						failures.Add(1)

						return
					}

					n, readErr := io.Copy(io.Discard, rc)

					_, closeErr := rc.Close(ctx)

					if readErr == nil && closeErr == nil && n == chunkSize {
						served.Add(1)
					} else {
						failures.Add(1)
					}
				})
			}

			batch.Wait()

			// Wait for this batch's fills before starting the next one: dropped
			// fills have no work, and draining here is what keeps the warm-up
			// under the queue's concurrency.
			c.wg.Wait()
		}

		require.Zero(t, failures.Load(), "every warm-up read must be served")
		require.EqualValues(t, chunks, served.Load(), "every warm-up read must be served")

		entries, err := os.ReadDir(cacheDir)
		require.NoError(t, err)

		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".bin") {
				landed++
			}
		}

		return landed
	}

	//nolint:paralleltest // the shapes share the process-wide writeback queue
	t.Run("steady warm-up warms the cache completely", func(t *testing.T) {
		// Start from a quiet process-wide queue so the shape below is what is
		// measured.
		writebacks.wg.Wait()

		landed := warm(t, 32) // well under maxConcurrentWritebacks
		require.Equal(t, chunks, landed,
			"a warm-up that stays under the queue's concurrency must not drop fills")
	})

	//nolint:paralleltest // the shapes share the process-wide writeback queue
	t.Run("saturated queue drops fills, which costs one refetch", func(t *testing.T) {
		writebacks.wg.Wait()

		ctx := t.Context()

		// A noisy tenant holds every slot, so this warm-up's fills find no room.
		release := make(chan struct{})

		for range maxConcurrentWritebacks {
			require.True(t, writebacks.submit(ctx, nil, func(context.Context) { <-release }))
		}

		landed := warm(t, maxConcurrentWritebacks)
		require.Zero(t, landed, "fills submitted to a saturated queue are dropped, not queued")

		close(release)

		writebacks.wg.Wait()

		// The dropped fills cost one refetch: the same chunks warm normally once
		// the queue has room again.
		landed = warm(t, 32)
		require.Equal(t, chunks, landed, "a dropped fill is refetched by the next read")
	})
}
