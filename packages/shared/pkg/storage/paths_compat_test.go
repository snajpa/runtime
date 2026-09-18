package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStoredArtifactPathsAreStable pins the storage-path contract for stored
// artifacts (S-55): the headers, compressed object names and uncompressed-size
// sidecars a build writes must keep the exact names existing snapshots already
// use — renaming any of them would orphan live data.
func TestStoredArtifactPathsAreStable(t *testing.T) {
	t.Parallel()

	paths := Paths{BuildID: "build-1"}

	require.Equal(t, "build-1", paths.StorageDir())
	require.Equal(t, "build-1", paths.CacheKey())

	require.Equal(t, "build-1/memfile", paths.Memfile())
	require.Equal(t, "build-1/memfile.header", paths.MemfileHeader())
	require.Equal(t, "build-1/rootfs.ext4", paths.Rootfs())
	require.Equal(t, "build-1/rootfs.ext4.header", paths.RootfsHeader())

	require.Equal(t, "build-1/memfile.zstd", paths.MemfileCompressed(CompressionZstd))
	require.Equal(t, "build-1/memfile.lz4", paths.MemfileCompressed(CompressionLZ4))

	// The size sidecars and header suffix are read by the FS backend and by
	// recoverable paths in other packages; keep them byte-identical.
	require.Equal(t, ".header", HeaderSuffix)
	require.Equal(t, "build-1/memfile.zstd.uncompressed-size",
		SizeSidecar(paths.MemfileCompressed(CompressionZstd)))
	require.Equal(t, "build-1/memfile.header", paths.HeaderFile(MemfileName))
}
