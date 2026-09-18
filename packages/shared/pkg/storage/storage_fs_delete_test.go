package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteObjectsWithPrefixDeletesExactlyTheSubtree pins the blast radius of
// an FS delete: the prefix's own subtree goes, and nothing whose name merely
// shares the prefix's characters (a sibling) does (REQ-B6, INV-8).
func TestDeleteObjectsWithPrefixDeletesExactlyTheSubtree(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "base")

	write := func(rel string) {
		t.Helper()

		full := filepath.Join(base, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0o644))
	}

	write("data/a.txt")
	write("data/sub/c.txt")
	write("data-sibling/keep.txt")
	write("other/keep.txt")

	p := newFileSystemStorage(base, "", nil)

	require.NoError(t, p.DeleteObjectsWithPrefix(t.Context(), "data"))

	_, err := os.Stat(filepath.Join(base, "data"))
	require.ErrorIs(t, err, os.ErrNotExist, "the deleted prefix must be gone")

	for _, keep := range []string{"data-sibling/keep.txt", "other/keep.txt"} {
		_, statErr := os.Stat(filepath.Join(base, filepath.FromSlash(keep)))
		assert.NoErrorf(t, statErr, "%s must survive a delete of the %q prefix", keep, "data")
	}
}

// TestDeleteObjectsWithPrefixUnlinksSymlinkedEntries documents the RemoveAll
// contract the FS delete relies on: an entry inside the deleted subtree that is
// a symlink is unlinked, never followed, so a link planted in the tree cannot
// widen the delete beyond the base (INV-8).
func TestDeleteObjectsWithPrefixUnlinksSymlinkedEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	base := filepath.Join(root, "base")
	outside := filepath.Join(root, "outside")

	require.NoError(t, os.MkdirAll(filepath.Join(base, "data"), 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "target.txt"), []byte("keep"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(base, "data", "link")))

	p := newFileSystemStorage(base, "", nil)

	require.NoError(t, p.DeleteObjectsWithPrefix(t.Context(), "data"))

	_, err := os.Stat(filepath.Join(base, "data"))
	require.ErrorIs(t, err, os.ErrNotExist, "the prefix subtree must be gone")

	_, err = os.Stat(filepath.Join(outside, "target.txt"))
	assert.NoError(t, err, "a symlinked entry must be unlinked, not followed into its target")
}
