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

// TestNFSCacheOpenRefusesEscapingPath: the cache joins caller paths onto its
// root, so a traversal-shaped path must fail before the inner provider is
// called and before any directory is created outside the root. The mock has no
// expectations, so a guard miss would fail loudly here instead of writing.
func TestNFSCacheOpenRefusesEscapingPath(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	provider := NewMockStorageProvider(t)

	c := &cache{rootPath: rootPath, chunkSize: MemoryChunkSize, inner: provider, tracer: noopTracer}

	for _, bad := range []string{"../evil", "a/../../evil", ""} {
		_, err := c.OpenBlob(t.Context(), bad)
		require.ErrorIsf(t, err, ErrInvalidStoragePath, "path %q", bad)

		_, err = c.OpenSeekable(t.Context(), bad)
		require.ErrorIsf(t, err, ErrInvalidStoragePath, "path %q", bad)
	}

	require.ErrorIs(t, c.DeleteObjectsWithPrefix(t.Context(), "../evil"), ErrInvalidStoragePath)

	_, statErr := os.Stat(filepath.Join(filepath.Dir(rootPath), "evil"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no path outside the root may be created")
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
