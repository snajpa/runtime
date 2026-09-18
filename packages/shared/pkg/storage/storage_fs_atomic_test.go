package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// S-01 regressions: the filesystem provider must truncate on rewrite, never
// create files on read, write atomically, and keep explicit permissions.

func newFSObject(t *testing.T, p *fsStorage, name string) *fsObject {
	t.Helper()

	obj, err := p.OpenBlob(t.Context(), name)
	require.NoError(t, err)

	fo, ok := obj.(*fsObject)
	require.True(t, ok, "FS provider must return *fsObject")

	return fo
}

func TestFSAtomicRewriteTruncates(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	fo := newFSObject(t, p, "layer")
	ctx := t.Context()

	require.NoError(t, fo.Put(ctx, []byte("a much longer first content")))
	require.NoError(t, fo.Put(ctx, []byte("short")))

	raw, err := os.ReadFile(filepath.Join(p.basePath, "layer"))
	require.NoError(t, err)
	assert.Equal(t, "short", string(raw))
}

func TestFSAtomicReadMissingDoesNotCreate(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	fo := newFSObject(t, p, "missing")
	ctx := t.Context()

	var buf bytes.Buffer
	_, err := fo.WriteTo(ctx, &buf)
	require.ErrorIs(t, err, ErrObjectNotExist)

	_, statErr := os.Stat(filepath.Join(p.basePath, "missing"))
	assert.True(t, os.IsNotExist(statErr), "read must not create the object")
}

func TestFSAtomicWriteFailureKeepsOriginal(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	fo := newFSObject(t, p, "layer")
	ctx := t.Context()

	require.NoError(t, fo.Put(ctx, []byte("original")))

	boom := errors.New("boom")
	err := fo.atomicWriteFile(0o644, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))

		return boom
	})
	require.ErrorIs(t, err, boom)

	raw, err := os.ReadFile(filepath.Join(p.basePath, "layer"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(raw))

	entries, err := os.ReadDir(p.basePath)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "."), "no temp files left behind: %s", e.Name())
	}
}

func TestFSAtomicConcurrentWritersNoTornContent(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	ctx := t.Context()

	payloads := []string{
		strings.Repeat("a", 4096),
		strings.Repeat("b", 4096),
		strings.Repeat("c", 4096),
	}

	var wg sync.WaitGroup
	for _, payload := range payloads {
		wg.Add(1)

		go func(payload string) {
			defer wg.Done()

			obj, err := p.OpenBlob(ctx, "race")
			if err != nil {
				return
			}

			_ = obj.Put(ctx, []byte(payload))
		}(payload)
	}
	wg.Wait()

	raw, err := os.ReadFile(filepath.Join(p.basePath, "race"))
	require.NoError(t, err)
	assert.Contains(t, payloads, string(raw), "content must be one writer's payload, never a mix")
}

func TestFSAtomicPermissionsExplicit(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	fo := newFSObject(t, p, "layer")

	require.NoError(t, fo.Put(t.Context(), []byte("x")))

	info, err := os.Stat(filepath.Join(p.basePath, "layer"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestFSAtomicSizePrefersSidecar(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	fo := newFSObject(t, p, "layer")
	ctx := t.Context()

	require.NoError(t, fo.Put(ctx, []byte("data")))

	sidecar := SizeSidecar(filepath.Join(p.basePath, "layer"))
	require.NoError(t, writeFileAtomic(sidecar, 0o644, func(w io.Writer) error {
		_, err := w.Write([]byte("12345"))

		return err
	}))

	size, err := fo.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(12345), size)
}

func TestFSAtomicReadOnlyFileReadable(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	p := newTempProvider(t)
	fo := newFSObject(t, p, "layer")
	ctx := t.Context()

	require.NoError(t, fo.Put(ctx, []byte("read me")))
	require.NoError(t, os.Chmod(filepath.Join(p.basePath, "layer"), 0o400))

	var buf bytes.Buffer
	n, err := fo.WriteTo(ctx, &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(len("read me")), n)
	assert.Equal(t, "read me", buf.String())
}
