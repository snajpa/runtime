package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// pacedReader yields one byte per Read after a delay.
type pacedReader struct {
	chunks int
	delay  time.Duration
	read   int
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if r.read >= r.chunks {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	r.read++
	p[0] = 'x'

	return 1, nil
}

func TestIdleDeadlineReaderKeepsProgressingTransferAlive(t *testing.T) {
	t.Parallel()

	const timeout = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	body := &pacedReader{chunks: 6, delay: 12 * time.Millisecond}
	r := newIdleDeadlineReader(io.NopCloser(body), timeout, cancel)
	defer r.stop()

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "xxxxxx", string(got))
	require.NoError(t, ctx.Err(), "progress must keep the transfer alive past the idle window")
}

func TestIdleDeadlineReaderCancelsStalledTransfer(t *testing.T) {
	t.Parallel()

	const timeout = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	r := newIdleDeadlineReader(io.NopCloser(&pacedReader{chunks: 1}), timeout, cancel)
	defer r.stop()

	buf := make([]byte, 1)
	_, err := r.Read(buf)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return ctx.Err() != nil }, time.Second, 5*time.Millisecond)
}

func TestIdleDeadlineReaderStopPreventsCancel(t *testing.T) {
	t.Parallel()

	const timeout = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	r := newIdleDeadlineReader(io.NopCloser(&pacedReader{chunks: 1}), timeout, cancel)
	r.stop()

	time.Sleep(3 * timeout)
	require.NoError(t, ctx.Err(), "stop must prevent the idle deadline from firing")
}
