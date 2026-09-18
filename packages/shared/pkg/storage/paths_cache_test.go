package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCachePaths(t *testing.T, buildID string) (Config, CachePaths) {
	t.Helper()

	config := Config{TemplateCacheDir: t.TempDir()}

	paths, err := Paths{BuildID: buildID}.Cache(config)
	require.NoError(t, err)

	return config, paths
}

func TestCacheDirPermissions(t *testing.T) {
	t.Parallel()

	_, paths := testCachePaths(t, uuid.NewString())
	t.Cleanup(func() { _ = paths.Close() })

	leaf, err := os.Stat(paths.cacheDir())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(CacheDirPerm), leaf.Mode().Perm(), "cache instance dir must be owner-only")

	parent, err := os.Stat(filepath.Dir(paths.cacheDir()))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(CacheDirPerm), parent.Mode().Perm(), "cache parent dir must be owner-only")
}

// TestCacheDirPermissionsTightenLegacyModes pins the create+chmod policy: a
// cache tree created by an older release with os.ModePerm (0777) must be
// tightened when the cache is opened again.
func TestCacheDirPermissionsTightenLegacyModes(t *testing.T) {
	t.Parallel()

	templateCacheDir := t.TempDir()
	buildID := uuid.NewString()
	cacheParent := filepath.Join(templateCacheDir, buildID, "cache")

	require.NoError(t, os.MkdirAll(cacheParent, 0o777))
	require.NoError(t, os.Chmod(cacheParent, 0o777))

	paths, err := Paths{BuildID: buildID}.Cache(Config{TemplateCacheDir: templateCacheDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = paths.Close() })

	info, err := os.Stat(cacheParent)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(CacheDirPerm), info.Mode().Perm())
}

func TestCacheCloseRemovesOnlyItsOwnInstance(t *testing.T) {
	t.Parallel()

	config, first := testCachePaths(t, uuid.NewString())
	second, err := Paths{BuildID: first.BuildID}.Cache(config)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	require.NoError(t, os.WriteFile(first.CacheMetadata(), []byte("first"), CacheFilePerm))
	require.NoError(t, os.WriteFile(second.CacheMetadata(), []byte("second"), CacheFilePerm))

	require.NoError(t, first.Close())

	_, err = os.Stat(first.cacheDir())
	assert.True(t, os.IsNotExist(err), "closed instance directory must be removed")

	content, err := os.ReadFile(second.CacheMetadata())
	require.NoError(t, err)
	assert.Equal(t, "second", string(content), "sibling instance data must survive")
}

func TestCacheCloseRefusesTamperedPaths(t *testing.T) {
	t.Parallel()

	config, victim := testCachePaths(t, uuid.NewString())
	t.Cleanup(func() { _ = victim.Close() })

	require.NoError(t, os.WriteFile(victim.CacheMetadata(), []byte("keep"), CacheFilePerm))

	tampered := CachePaths{
		Paths:           Paths{BuildID: victim.BuildID},
		CacheIdentifier: "..",
		config:          config,
	}
	require.Error(t, tampered.Close(), "a non-UUID identifier must be refused")

	incomplete := CachePaths{Paths: Paths{}, CacheIdentifier: uuid.NewString()}
	require.Error(t, incomplete.Close(), "an incomplete cache path must be refused")

	_, err := os.Stat(victim.CacheMetadata())
	require.NoError(t, err, "refused closes must not remove anything")
}

func TestCacheCloseRefusesNonDirectory(t *testing.T) {
	t.Parallel()

	config := Config{TemplateCacheDir: t.TempDir()}
	identifier := uuid.NewString()
	dir := filepath.Join(config.TemplateCacheDir, "build", "cache", identifier)
	require.NoError(t, os.MkdirAll(filepath.Dir(dir), CacheDirPerm))
	require.NoError(t, os.WriteFile(dir, []byte("not a directory"), CacheFilePerm))

	paths := CachePaths{
		Paths:           Paths{BuildID: "build"},
		CacheIdentifier: identifier,
		config:          config,
	}
	require.Error(t, paths.Close())

	_, err := os.Stat(dir)
	require.NoError(t, err, "a non-directory cache path must not be removed")
}

func TestCacheCloseReportsMissingDirectory(t *testing.T) {
	t.Parallel()

	_, paths := testCachePaths(t, uuid.NewString())
	require.NoError(t, os.RemoveAll(paths.cacheDir()))

	require.Error(t, paths.Close(), "a missing cache directory must not be skipped silently")
}
