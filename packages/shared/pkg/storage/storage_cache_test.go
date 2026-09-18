package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNFSCacheDirPermissions(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()

	provider := NewMockStorageProvider(t)
	provider.EXPECT().OpenBlob(mock.Anything, "blob-object").Return(NewMockBlob(t), nil)
	provider.EXPECT().OpenSeekable(mock.Anything, "seekable-object").Return(NewMockSeekable(t), nil)

	c := &cache{rootPath: rootPath, chunkSize: MemoryChunkSize, inner: provider, tracer: noopTracer}

	_, err := c.OpenBlob(t.Context(), "blob-object")
	require.NoError(t, err)

	_, err = c.OpenSeekable(t.Context(), "seekable-object")
	require.NoError(t, err)

	for _, object := range []string{"blob-object", "seekable-object"} {
		info, err := os.Stat(filepath.Join(rootPath, object))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(CacheDirPerm), info.Mode().Perm(), "object cache dir for %s must be owner-only", object)
	}
}

// TestNFSCacheDirPermissionsTightenLegacyModes pins the create+chmod policy:
// an object cache directory left world-writable by an older release must be
// tightened when the object is opened again.
func TestNFSCacheDirPermissionsTightenLegacyModes(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	localPath := filepath.Join(rootPath, "object")

	require.NoError(t, os.MkdirAll(localPath, 0o777))
	require.NoError(t, os.Chmod(localPath, 0o777))

	provider := NewMockStorageProvider(t)
	provider.EXPECT().OpenBlob(mock.Anything, "object").Return(NewMockBlob(t), nil)

	c := &cache{rootPath: rootPath, chunkSize: MemoryChunkSize, inner: provider, tracer: noopTracer}

	_, err := c.OpenBlob(t.Context(), "object")
	require.NoError(t, err)

	info, err := os.Stat(localPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(CacheDirPerm), info.Mode().Perm())
}
