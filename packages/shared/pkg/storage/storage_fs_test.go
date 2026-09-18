package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to create a FileSystemStorageProvider rooted in a temp directory.
func newTempProvider(t *testing.T) *fsStorage {
	t.Helper()

	base := t.TempDir()
	p := newFileSystemStorage(base, "", nil)

	return p
}

func TestOpenObject_Write_Exists_WriteTo(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	obj, err := p.OpenBlob(ctx, filepath.Join("sub", "file.txt"))
	require.NoError(t, err)

	contents := []byte("hello world")
	// write via Write
	err = obj.Put(t.Context(), contents)
	require.NoError(t, err)

	// check Size
	exists, err := obj.Exists(t.Context())
	require.NoError(t, err)
	require.True(t, exists)

	// read the entire file back via WriteTo
	data, err := GetBlob(t.Context(), obj)
	require.NoError(t, err)
	require.Equal(t, contents, data)
}

func TestFSPut(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	// create a separate source file on disk
	srcPath := filepath.Join(t.TempDir(), "src.txt")
	const payload = "copy me please"
	require.NoError(t, os.WriteFile(srcPath, []byte(payload), 0o600))

	obj, err := p.OpenBlob(ctx, "copy/dst.txt")
	require.NoError(t, err)

	require.NoError(t, obj.Put(t.Context(), []byte(payload)))

	data, err := GetBlob(t.Context(), obj)
	require.NoError(t, err)
	require.Equal(t, payload, string(data))
}

func TestDelete(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	obj, err := p.OpenBlob(ctx, "to/delete.txt")
	require.NoError(t, err)

	err = obj.Put(t.Context(), []byte("bye"))
	require.NoError(t, err)

	exists, err := obj.Exists(t.Context())
	require.NoError(t, err)
	assert.True(t, exists)

	err = p.DeleteObjectsWithPrefix(t.Context(), "to/delete.txt")
	require.NoError(t, err)

	// subsequent Size call should fail with ErrorObjectNotExist
	exists, err = obj.Exists(t.Context())
	require.NoError(t, err)
	assert.False(t, exists)
}

// TestDeleteObjectsWithPrefixRefusesUnsafePrefix: an empty prefix used to
// resolve to the base directory itself and RemoveAll it, and a ".."-bearing
// prefix used to resolve outside the base. Both must fail and leave the tree
// intact.
func TestDeleteObjectsWithPrefixRefusesUnsafePrefix(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "build-id"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "build-id", "memfile"), []byte("data"), 0o644))

	outside := filepath.Join(filepath.Dir(base), "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o644))

	p := newFileSystemStorage(base, "", nil)

	for _, prefix := range []string{"", "..", "../outside", "build-id/../../outside"} {
		require.ErrorIsf(t, p.DeleteObjectsWithPrefix(t.Context(), prefix), ErrInvalidStoragePath, "prefix %q", prefix)
	}

	entries, err := os.ReadDir(filepath.Join(base, "build-id"))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the base tree must be intact after the refusals")

	_, err = os.Stat(filepath.Join(outside, "keep"))
	assert.NoError(t, err, "nothing outside the base may be deleted")
}

// TestOpenRefusesEscapingPath: a traversal-shaped object path fails before any
// directory or file is created outside the base.
func TestOpenRefusesEscapingPath(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(base, 0o755))

	p := newFileSystemStorage(base, "", nil)

	_, err := p.OpenBlob(t.Context(), "../evil")
	require.ErrorIs(t, err, ErrInvalidStoragePath)

	_, err = p.OpenSeekable(t.Context(), "a/../../evil")
	require.ErrorIs(t, err, ErrInvalidStoragePath)

	_, statErr := os.Stat(filepath.Join(filepath.Dir(base), "evil"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no path outside the base may be created")
}

func TestDeleteObjectsWithPrefix(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	paths := []string{
		"data/a.txt",
		"data/b.txt",
		"data/sub/c.txt",
	}
	for _, pth := range paths {
		obj, err := p.OpenBlob(ctx, pth)
		require.NoError(t, err)
		err = obj.Put(t.Context(), []byte("x"))
		require.NoError(t, err)
	}

	// remove the entire "data" prefix
	require.NoError(t, p.DeleteObjectsWithPrefix(ctx, "data"))

	for _, pth := range paths {
		full := filepath.Join(p.GetDetails()[len("[Local file storage, base path set to "):len(p.GetDetails())-1], pth) // derive basePath
		_, err := os.Stat(full)
		require.True(t, os.IsNotExist(err))
	}
}

func TestWriteToNonExistentObject(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)

	ctx := t.Context()
	obj, err := p.OpenBlob(ctx, "missing/file.txt")
	require.NoError(t, err)

	_, err = GetBlob(t.Context(), obj)
	require.ErrorIs(t, err, ErrObjectNotExist)
}

// The filesystem uploader streams parts into a same-directory temp file and
// commits it atomically on Complete, so a failed upload leaves no file behind
// and memory stays O(part) (REQ-B4).
func TestFSPartUploaderResidueContract(t *testing.T) {
	t.Parallel()

	uploader := &fsPartUploader{}
	require.True(t, uploader.Abortable())
	require.Equal(t, "fs", uploader.ProviderName())
}

// Parts must be assembled in part order even when they arrive out of order,
// and no temp file may survive a completed upload.
func TestFSPartUploaderStreamsPartsInOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "object.bin")
	u := &fsPartUploader{fullPath: target}

	require.NoError(t, u.Start(t.Context()))
	require.NoError(t, u.UploadPart(t.Context(), 2, []byte("two")))
	require.NoError(t, u.UploadPart(t.Context(), 1, []byte("one"), []byte("-split")))
	require.NoError(t, u.Complete(t.Context()))

	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "one-splittwo", string(content))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the committed object may remain")
}

// Close discards the staged temp file and leaves the target untouched.
func TestFSPartUploaderAbortRemovesTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "object.bin")
	u := &fsPartUploader{fullPath: target}

	require.NoError(t, u.Start(t.Context()))
	require.NoError(t, u.UploadPart(t.Context(), 1, []byte("partial")))
	require.NoError(t, u.Close())

	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the temp file must be removed on abort")
}

// A compressed filesystem store must round-trip through the streaming
// uploader without assembling the object in memory.
func TestFSCompressedStoreFileRoundTrip(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)

	data := generateSemiRandomData(3 * megabyte)
	inputPath := filepath.Join(t.TempDir(), "input.bin")
	require.NoError(t, os.WriteFile(inputPath, data, 0o600))

	obj, err := p.OpenSeekable(t.Context(), "layer/data.bin")
	require.NoError(t, err)

	ft, _, err := obj.StoreFile(t.Context(), inputPath, WithCompressConfig(defaultCfg(CompressionZstd, 2, 512*1024)))
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(p.basePath, "layer", "data.bin"))
	require.NoError(t, err)

	got, err := decompressAll(ft.Table(), raw)
	require.NoError(t, err)
	require.Equal(t, data, got)
}
