package storage

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileSectionReaderStreamsSection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "section.bin")
	require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0o600))

	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	r := newFileSectionReader(f, 2, 4)
	require.Equal(t, 4, r.Len(), "Len must mirror the unread section for Content-Length")

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "2345", string(got))
	require.Equal(t, 0, r.Len())

	// Retries replay the body: a fresh reader starts at the section's start.
	replay := newFileSectionReader(f, 2, 4)
	got, err = io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, "2345", string(got))
}
