package storage

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/units"
)

// TestMemoryChunkSizeCoversBlockSizes pins storage.go's "must always be bigger
// or equal to the block size" contract for the memory chunk.
func TestMemoryChunkSizeCoversBlockSizes(t *testing.T) {
	t.Parallel()

	require.GreaterOrEqual(t, MemoryChunkSize, units.HugepageSize)
	require.Zero(t, MemoryChunkSize%units.HugepageSize)
	require.Zero(t, MemoryChunkSize%units.PageSize)
}

// TestDefaultCompressFrameSizeCoversBlockSizes pins compress_config.go's "MUST
// be multiple of every block/page size" contract for the default frame.
func TestDefaultCompressFrameSizeCoversBlockSizes(t *testing.T) {
	t.Parallel()

	frame := CompressConfig{Enabled: true, Type: "zstd"}.FrameSize()
	require.Equal(t, DefaultCompressFrameSize, frame)
	require.Zero(t, frame%units.HugepageSize, "default frame must be a multiple of the UFFD huge-page size")
	require.Zero(t, frame%units.RootfsBlockSize, "default frame must be a multiple of the rootfs block size")
}
