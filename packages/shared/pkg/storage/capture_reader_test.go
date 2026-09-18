package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeErrReader is a RangeReader whose Close always fails, for the capture
// reader's failure path.
type closeErrReader struct {
	io.Reader
}

func (closeErrReader) Close(context.Context) (*ReadStats, error) {
	return nil, errors.New("close failed")
}

// TestCaptureReaderPooledReleaseIsIdempotent pins the ownership handoff: the
// payload's release returns the pooled buffer and is safe to call more than
// once.
func TestCaptureReaderPooledReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte{0xAB}, 4096)

	var captured capturedBytes

	r := newCaptureReader(bytesRangeReader(data), len(data), false, func(_ context.Context, c capturedBytes) {
		captured = c
	})

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	mustClose(t, r)

	require.Equal(t, data, captured.Bytes())
	captured.Release()
	captured.Release() // must not panic or double-free
}

// TestCaptureReaderGrowsBeyondPooledCapacity pins that a payload larger than
// the pooled hint is still captured correctly and released once.
func TestCaptureReaderGrowsBeyondPooledCapacity(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte{0x5A}, 1024)

	var captured capturedBytes

	r := newCaptureReader(bytesRangeReader(data), 16, false, func(_ context.Context, c capturedBytes) {
		captured = c
	})

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	mustClose(t, r)

	require.Equal(t, data, captured.Bytes())
	captured.Release()
}

// TestCaptureReaderWithoutHintUsesPlainBuffer pins the no-pool path: a capture
// without a size hint still works and its release is a no-op.
func TestCaptureReaderWithoutHintUsesPlainBuffer(t *testing.T) {
	t.Parallel()

	data := []byte("tiny")

	var captured capturedBytes

	r := newCaptureReader(bytesRangeReader(data), 0, false, func(_ context.Context, c capturedBytes) {
		captured = c
	})

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	mustClose(t, r)

	require.Equal(t, data, captured.Bytes())
	captured.Release()
}

// TestCaptureReaderCloseErrorDoesNotHandOutPayload pins the failure path: when
// the inner close fails, the callback is never invoked (nothing consumes the
// capture) and the buffer is returned instead.
func TestCaptureReaderCloseErrorDoesNotHandOutPayload(t *testing.T) {
	t.Parallel()

	data := []byte("payload")

	var called bool

	r := newCaptureReader(closeErrReader{bytes.NewReader(data)}, len(data), false,
		func(_ context.Context, _ capturedBytes) { called = true })

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	_, err = r.Close(t.Context())
	require.Error(t, err)
	assert.False(t, called, "a failed close must not hand out the capture")
}
