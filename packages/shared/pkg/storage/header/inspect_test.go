package header

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestValidateMappingsEmpty pins the never-index guarantee: an empty mapping set
// with a non-zero size is a malformed header and must fail, not panic. (The old
// code indexed mappings[len(mappings)-1] and panicked on exactly this input.)
func TestValidateMappingsEmpty(t *testing.T) {
	t.Parallel()

	require.Error(t, ValidateMappings(nil, blockSize, blockSize))
	require.NoError(t, ValidateMappings(nil, 0, blockSize), "an empty set covers a zero-size artifact")
}

// TestVisualizeBounds pins the bounded grid: size, blockSize and cols come from
// the artifact under inspection, so hostile values must fail with an error
// instead of allocating unboundedly, looping forever, or writing out of range.
func TestVisualizeBounds(t *testing.T) {
	t.Parallel()

	// A 1 TiB artifact at 4 KiB blocks is ~268M cells, far over the limit.
	_, err := Visualize(nil, 1<<40, 1<<12, 128, nil, nil)
	require.Error(t, err, "an oversized grid must be refused")

	_, err = Visualize(nil, blockSize, 0, 128, nil, nil)
	require.Error(t, err, "a zero block size must be refused")

	_, err = Visualize(nil, blockSize, blockSize, 0, nil, nil)
	require.Error(t, err, "a zero column count must be refused")

	_, err = Visualize(nil, blockSize+1, blockSize, 128, nil, nil)
	require.Error(t, err, "a size that is not block-aligned must be refused")

	build := uuid.New()

	_, err = Visualize([]BuildMap{{Offset: blockSize, Length: blockSize, BuildId: build}}, blockSize, blockSize, 128, nil, nil)
	require.Error(t, err, "a mapping running past the grid must be refused")

	view, err := Visualize(
		[]BuildMap{{Offset: 0, Length: blockSize, BuildId: build}},
		blockSize, blockSize, 128, nil,
		map[uuid.UUID]struct{}{build: {}},
	)
	require.NoError(t, err)
	require.Equal(t, string(DirtyBlockChar2), view)
}

// TestMappingEqualIncludesStorageOffset pins that two mappings with the same
// device range and build but different storage offsets are not equal: they
// resolve to different bytes in the build's data.
func TestMappingEqualIncludesStorageOffset(t *testing.T) {
	t.Parallel()

	build := uuid.New()
	a := BuildMap{Offset: 0, Length: blockSize, BuildId: build, BuildStorageOffset: 0}
	b := BuildMap{Offset: 0, Length: blockSize, BuildId: build, BuildStorageOffset: blockSize}

	require.False(t, a.Equal(b))
	require.False(t, Equal([]BuildMap{a}, []BuildMap{b}))

	same := BuildMap{Offset: 0, Length: blockSize, BuildId: build, BuildStorageOffset: 0}
	require.True(t, a.Equal(same))
	require.True(t, Equal([]BuildMap{a}, []BuildMap{same}))
}
