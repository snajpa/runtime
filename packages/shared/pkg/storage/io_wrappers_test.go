package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	// The transfer must outlive the idle window (so the property is exercised)
	// while every progress gap stays far below it (so scheduler jitter under
	// load cannot look like a stall). The dev-environment validation tripped
	// the previous 40 ms window with 12 ms progress (2026-09-19); this is
	// ~1.5 s of 10 ms progress against a 1 s window (~100x headroom per gap).
	const (
		timeout = 1 * time.Second
		chunks  = 150
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	body := &pacedReader{chunks: chunks, delay: 10 * time.Millisecond}
	r := newIdleDeadlineReader(io.NopCloser(body), timeout, cancel)
	defer r.stop()

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("x", chunks), string(got))
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
