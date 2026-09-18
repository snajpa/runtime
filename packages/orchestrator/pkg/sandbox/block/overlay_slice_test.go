//go:build linux

package block

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// Slice resolves through the same layered chain as ReadAt (writable cache →
// sealing cache → base device), clamps to the device size and returns a
// caller-owned copy (REQ-D4/5; audit §8.1).
func TestOverlaySliceResolvesLayersAndClamps(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)
	numBlocks := int64(4)

	baseData := make([]byte, blockSize*numBlocks)
	copy(baseData[3*blockSize:4*blockSize], fill(blockSize, 0xCC))
	base := &fakeOriginalDevice{data: baseData}

	cache, err := NewCache(blockSize*numBlocks, blockSize, t.TempDir()+"/c0", false)
	require.NoError(t, err)

	o := NewOverlay(base, cache)
	t.Cleanup(func() { require.NoError(t, o.Close()) })

	_, err = o.WriteAt(fill(blockSize, 0xAA), 0)
	require.NoError(t, err)

	// Cached block.
	got, err := o.Slice(t.Context(), 0, blockSize)
	require.NoError(t, err)
	require.Equal(t, fill(blockSize, 0xAA), got)

	// Range spanning an untouched block and the patterned base-backed block.
	got, err = o.Slice(t.Context(), 2*blockSize, 2*blockSize)
	require.NoError(t, err)
	require.Len(t, got, int(2*blockSize))
	require.Equal(t, make([]byte, blockSize), got[:blockSize], "an untouched block reads as zeros")
	require.Equal(t, fill(blockSize, 0xCC), got[blockSize:], "the last block resolves via the base device")

	// Over-long ranges clamp at the device end instead of failing.
	got, err = o.Slice(t.Context(), blockSize*3, blockSize*4)
	require.NoError(t, err)
	require.Len(t, got, int(blockSize))

	// A range starting past the end is empty, not a panic.
	got, err = o.Slice(t.Context(), blockSize*numBlocks+blockSize, blockSize)
	require.NoError(t, err)
	require.Empty(t, got)

	// The caller owns the returned bytes: mutating them must not touch the
	// device.
	got, err = o.Slice(t.Context(), blockSize*3, blockSize)
	require.NoError(t, err)
	got[0] ^= 0xFF

	again, err := o.Slice(t.Context(), blockSize*3, blockSize)
	require.NoError(t, err)
	require.Equal(t, fill(blockSize, 0xCC), again, "Slice must return a copy")
}

// Partial-block ranges must resolve via one aligned backing read and a trim:
// the pre-fix code allocated exactly the clamped length and sliced whole
// blocks out of it, so any byte-granular range panicked (reviewer0's probe,
// S-24). These cases pin the start, the internal straddle, the clamped tail
// and a direct partial ReadAt.
func TestOverlaySlicePartialBlockRanges(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)
	numBlocks := int64(4)
	size := blockSize * numBlocks

	baseData := make([]byte, size)
	copy(baseData[:blockSize], fill(blockSize, 0x11))
	copy(baseData[blockSize:2*blockSize], fill(blockSize, 0x22))
	copy(baseData[3*blockSize:], fill(blockSize, 0xCC))
	base := &fakeOriginalDevice{data: baseData}

	cache, err := NewCache(size, blockSize, t.TempDir()+"/c0", false)
	require.NoError(t, err)

	o := NewOverlay(base, cache)
	t.Cleanup(func() { require.NoError(t, o.Close()) })

	_, err = o.WriteAt(fill(blockSize, 0xAA), 0)
	require.NoError(t, err)

	// One byte at the start of a cached block.
	got, err := o.Slice(t.Context(), 0, 1)
	require.NoError(t, err)
	require.Equal(t, []byte{0xAA}, got)

	// Straddles the cached/base boundary: last cached byte + first base byte.
	got, err = o.Slice(t.Context(), blockSize-1, 2)
	require.NoError(t, err)
	require.Equal(t, []byte{0xAA, 0x22}, got)

	// Ends mid-block and clamps at the device end.
	got, err = o.Slice(t.Context(), size-1, 2)
	require.NoError(t, err)
	require.Equal(t, []byte{0xCC}, got)

	// A partial range inside a single block.
	got, err = o.Slice(t.Context(), 3, 5)
	require.NoError(t, err)
	require.Equal(t, []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, got)

	// Direct ReadAt with a non-block-multiple buffer and an aligned offset must
	// not panic either.
	buf := make([]byte, 3)
	n, err := o.ReadAt(t.Context(), buf, blockSize)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Equal(t, []byte{0x22, 0x22, 0x22}, buf)
}
