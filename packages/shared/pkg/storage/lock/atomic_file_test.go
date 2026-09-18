package lock

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenFile(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()

		expected := []byte("hello")

		tempDir := t.TempDir()
		filename := filepath.Join(tempDir, "test.bin")

		f, err := OpenFile(t.Context(), filename)
		require.NoError(t, err)
		require.NotNil(t, f)

		count, err := f.Write(expected)
		require.NoError(t, err)
		assert.Equal(t, len(expected), count)

		_, err = os.Stat(filename)
		require.Error(t, err)
		assert.True(t, os.IsNotExist(err))

		err = f.Commit(t.Context())
		require.NoError(t, err)

		data, err := os.ReadFile(filename)
		require.NoError(t, err)
		assert.Equal(t, expected, data)
	})

	t.Run("close without commit drops new data", func(t *testing.T) {
		t.Parallel()

		expected := []byte("hello")

		tempDir := t.TempDir()
		filename := filepath.Join(tempDir, "test.bin")

		f, err := OpenFile(t.Context(), filename)
		require.NoError(t, err)
		require.NotNil(t, f)

		count, err := f.Write(expected)
		require.NoError(t, err)
		assert.Equal(t, len(expected), count)

		_, err = os.Stat(filename)
		require.ErrorIs(t, err, os.ErrNotExist)

		err = f.Close(t.Context())
		require.NoError(t, err)

		// destination file does not exist
		_, err = os.Stat(filename)
		require.ErrorIs(t, err, os.ErrNotExist)

		// temp file also does not exist
		_, err = os.Stat(f.tempFile.Name())
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("two files cannot be opened at the same time", func(t *testing.T) {
		t.Parallel()

		tempDir := t.TempDir()
		filename := filepath.Join(tempDir, "test.bin")

		f1, err := OpenFile(t.Context(), filename)
		require.NoError(t, err)
		t.Cleanup(func() {
			err := f1.Close(context.WithoutCancel(t.Context()))
			assert.NoError(t, err)
		})

		f2, err := OpenFile(t.Context(), filename)
		require.ErrorIs(t, err, ErrLockAlreadyHeld)
		assert.Nil(t, f2)

		err = f1.Close(t.Context())
		require.NoError(t, err)

		f2, err = OpenFile(t.Context(), filename)
		require.NoError(t, err)

		err = f2.Close(t.Context())
		assert.NoError(t, err)
	})

	t.Run("missing directory returns error", func(t *testing.T) {
		t.Parallel()

		tempDir := t.TempDir()
		filename := filepath.Join(tempDir, "a", "b", "test.bin")

		_, err := OpenFile(t.Context(), filename)
		require.Error(t, err)
		assert.ErrorIs(t, err, fs.ErrNotExist)
	})
}

// TestOpenFileCommitPublishesAndRemovesTemp pins the synced publish path: the
// commit leaves the final file with the written bytes and no temp file behind.
func TestOpenFileCommitPublishesAndRemovesTemp(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	filename := filepath.Join(tempDir, "test.bin")

	f, err := OpenFile(t.Context(), filename)
	require.NoError(t, err)

	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)

	tempName := f.tempFile.Name()

	require.NoError(t, f.Commit(t.Context()))

	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), data)

	_, err = os.Stat(tempName)
	assert.True(t, os.IsNotExist(err), "commit must remove the temp file")
}

// TestOpenFileFirstWriterWins pins the immutable-file contract: a later writer
// never overwrites the first one, and losing the commit race is a successful
// dedup by default.
func TestOpenFileFirstWriterWins(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	filename := filepath.Join(tempDir, "test.bin")

	first, err := OpenFile(t.Context(), filename)
	require.NoError(t, err)

	_, err = first.Write([]byte("first"))
	require.NoError(t, err)
	require.NoError(t, first.Commit(t.Context()))

	second, err := OpenFile(t.Context(), filename)
	require.NoError(t, err)

	_, err = second.Write([]byte("second"))
	require.NoError(t, err)
	require.NoError(t, second.Commit(t.Context()))

	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), data, "the first writer's bytes stay")
}

// TestOpenFileContentCheckMatching pins that an opted-in writer that wrote the
// same bytes as the winner still commits successfully (dedup confirmed).
func TestOpenFileContentCheckMatching(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	filename := filepath.Join(tempDir, "test.bin")

	first, err := OpenFile(t.Context(), filename)
	require.NoError(t, err)

	_, err = first.Write([]byte("same"))
	require.NoError(t, err)
	require.NoError(t, first.Commit(t.Context()))

	second, err := OpenFile(t.Context(), filename, WithContentCheck())
	require.NoError(t, err)

	_, err = second.Write([]byte("same"))
	require.NoError(t, err)
	require.NoError(t, second.Commit(t.Context()), "identical content is a successful dedup")

	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	assert.Equal(t, []byte("same"), data)
}

// TestOpenFileContentCheckMismatch pins that an opted-in writer whose bytes
// differ from the winning file gets ErrContentMismatch instead of a silent
// success — and never overwrites the winner.
func TestOpenFileContentCheckMismatch(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	filename := filepath.Join(tempDir, "test.bin")

	first, err := OpenFile(t.Context(), filename)
	require.NoError(t, err)

	_, err = first.Write([]byte("first"))
	require.NoError(t, err)
	require.NoError(t, first.Commit(t.Context()))

	second, err := OpenFile(t.Context(), filename, WithContentCheck())
	require.NoError(t, err)

	_, err = second.Write([]byte("second"))
	require.NoError(t, err)
	require.ErrorIs(t, second.Commit(t.Context()), ErrContentMismatch)

	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), data, "a mismatch never overwrites the winner")
}
