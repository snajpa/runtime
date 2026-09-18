//go:build linux

package userfaultfd

import (
	"slices"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TestPackedPageSinkMatchesBitmapRank pins the S-29 slot lookup against the
// bitmap's own rank convention: the precomputed ascending page list must map
// every set page to Rank-1 of that page.
func TestPackedPageSinkMatchesBitmapRank(t *testing.T) {
	t.Parallel()

	const ps = int64(header.PageSize)

	pages := roaring.New()
	for i := uint32(0); i < 4096; i += 3 {
		pages.Add(i)
	}
	pages.Add(1)
	pages.Add(4095)

	sink := NewPackedPageSink(pages, ps, newMemSink(int64(pages.GetCardinality())*ps))

	it := pages.Iterator()
	for it.HasNext() {
		idx := it.Next()
		slot, ok := slices.BinarySearch(sink.packed, idx)
		require.True(t, ok, "page %d missing from the precomputed list", idx)
		require.Equal(t, int(pages.Rank(idx)-1), slot, "slot of page %d must match Rank-1", idx)
	}

	// Pages outside the set stay rejected.
	_, err := sink.WriteAt(make([]byte, ps), 2*ps)
	require.Error(t, err, "page 2 is not in the set")
}
