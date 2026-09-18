package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateRelativePath pins the storage boundary: only names that resolve
// under a base directory pass.
func TestValidateRelativePath(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{
		"memfile",
		"build-id/memfile",
		"build-id//memfile",  // an empty element normalizes
		"build-id/./memfile", // "." normalizes
		"build-id/memfile/",  // a trailing separator normalizes
		"6c1f0a3e-3a4f-4d2c-9f0e-1b2a3c4d5e6f/memfile",
	} {
		require.NoErrorf(t, ValidateRelativePath(ok), "%q must be accepted", ok)
	}

	for _, bad := range []string{
		"",
		".",
		"..",
		"a/../b",
		"../a",
		"/etc/passwd",
		"C:/evil",
		"C:evil",
		"a\x00b",
		`..\evil`,
		`a\b`,
	} {
		assert.Errorf(t, ValidateRelativePath(bad), "%q must be rejected", bad)
	}
}

// TestContainedPath pins the join: the result stays under the base, and every
// escaping relation fails before the caller can touch the filesystem.
func TestContainedPath(t *testing.T) {
	t.Parallel()

	base := t.TempDir()

	joined, err := ContainedPath(base, "build-id/memfile")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "build-id", "memfile"), joined)

	joined, err = ContainedPath(base, "build-id/memfile/")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "build-id", "memfile"), joined, "a trailing separator normalizes")

	for _, bad := range []string{"", ".", "..", "../evil", "a/../../evil", "/etc/passwd"} {
		_, err := ContainedPath(base, bad)
		assert.ErrorIs(t, err, ErrInvalidStoragePath, "%q must be rejected", bad)
	}
}

// TestDeletePrefixGuardsRunBeforeProviderWork: every provider refuses an empty
// or escaping prefix before touching its client, so a bad prefix can never
// reach a bucket (or a zero-value client in this test).
func TestDeletePrefixGuardsRunBeforeProviderWork(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	providers := map[string]func(string) error{
		"fs": func(prefix string) error {
			return (&fsStorage{basePath: t.TempDir()}).DeleteObjectsWithPrefix(ctx, prefix)
		},
		"gcp":   func(prefix string) error { return (&gcpStorage{}).DeleteObjectsWithPrefix(ctx, prefix) },
		"aws":   func(prefix string) error { return (&awsStorage{}).DeleteObjectsWithPrefix(ctx, prefix) },
		"azure": func(prefix string) error { return (&azureStorage{}).DeleteObjectsWithPrefix(ctx, prefix) },
	}

	for name, del := range providers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, prefix := range []string{"", "..", "../evil", "build-id/../../evil"} {
				assert.ErrorIsf(t, del(prefix), ErrInvalidStoragePath, "prefix %q", prefix)
			}
		})
	}
}
