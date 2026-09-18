//go:build linux

package block

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// newWarmedChunker returns a chunker whose cache already holds one frame of
// data (frameSizeKB uncompressed), the upstream that served it, the frame
// table, and the frame's uncompressed length, so benchmarks and tests exercise
// the cache-hit path only.
func newWarmedChunker(tb testing.TB, frameSizeKB int) (*Chunker, storage.RangeOpener, *storage.FrameTable, int64) {
	tb.Helper()

	frameSize := int64(frameSizeKB) * 1024
	data := makeTestData(int(frameSize))

	ft, compressed, _, err := storage.CompressBytes(tb.Context(), data, storage.CompressConfig{
		Enabled:            true,
		Type:               "lz4",
		EncoderConcurrency: 1,
		FrameEncodeWorkers: 1,
		FrameSizeKB:        frameSizeKB,
		MinPartSizeMB:      50,
	})
	require.NoError(tb, err)

	upstream := &fakeSeekable{data: compressed}
	table := ft.Table()

	chunker, err := NewChunker(&featureflags.Client{}, frameSize, testBlockSize, tb.TempDir()+"/cache", newTestMetrics(tb), storage.MemfileObjectType)
	require.NoError(tb, err)

	// Warm the cache: the first slice fetches the frame and fills the mmap, so
	// every measured call is served from the cache.
	_, err = chunker.Slice(tb.Context(), 0, frameSize, upstream, table)
	require.NoError(tb, err)

	return chunker, upstream, table, frameSize
}

// BenchmarkChunkerCacheHitSlice measures the cache-hit read that hands the
// caller its own copy of the chunk (S-17's ownership model, INV-4).
func BenchmarkChunkerCacheHitSlice(b *testing.B) {
	for _, frameSizeKB := range []int{testFrameSize / 1024, 4096} {
		b.Run(fmt.Sprintf("frame_%dKB", frameSizeKB), func(b *testing.B) {
			chunker, upstream, table, frameSize := newWarmedChunker(b, frameSizeKB)
			defer chunker.Close()

			b.SetBytes(frameSize)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if _, err := chunker.Slice(b.Context(), 0, frameSize, upstream, table); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkChunkerCacheHitReadAt measures the same cache hit through ReadAt,
// which copies the chunk into the caller's buffer: on this path the owned copy
// inside the cache is a second, avoidable full-chunk copy.
func BenchmarkChunkerCacheHitReadAt(b *testing.B) {
	for _, frameSizeKB := range []int{testFrameSize / 1024, 4096} {
		b.Run(fmt.Sprintf("frame_%dKB", frameSizeKB), func(b *testing.B) {
			chunker, upstream, table, frameSize := newWarmedChunker(b, frameSizeKB)
			defer chunker.Close()

			buf := make([]byte, frameSize)

			b.SetBytes(frameSize)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if _, err := chunker.ReadAt(b.Context(), buf, 0, upstream, table); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCacheHitCopyVsLease isolates the copy cost: Slice returns an owned
// copy, while addressBytes lends the mapping under the cache's read lease and
// copies nothing. The difference is what a lease-style borrow API would save
// per cache-hit read of this size.
func BenchmarkCacheHitCopyVsLease(b *testing.B) {
	for _, frameSizeKB := range []int{testFrameSize / 1024, 4096} {
		chunker, _, _, frameSize := newWarmedChunker(b, frameSizeKB)

		b.Run(fmt.Sprintf("copy_%dKB", frameSizeKB), func(b *testing.B) {
			b.SetBytes(frameSize)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if _, err := chunker.cache.Slice(0, frameSize); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("lease_%dKB", frameSizeKB), func(b *testing.B) {
			b.SetBytes(frameSize)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				alias, release, err := chunker.cache.addressBytes(0, frameSize)
				if err != nil {
					b.Fatal(err)
				}

				_ = alias

				release()
			}
		})

		chunker.Close()
	}
}
