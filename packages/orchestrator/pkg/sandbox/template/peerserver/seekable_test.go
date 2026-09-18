//go:build linux

package peerserver

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	buildmocks "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build/mocks"
	peerservermocks "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template/peerserver/mocks"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func TestSeekableSource_Size(t *testing.T) {
	t.Parallel()

	diff := buildmocks.NewMockDiff(t)
	diff.EXPECT().Size(mock.Anything).Return(int64(1234), nil)

	cache := peerservermocks.NewMockCache(t)
	cache.EXPECT().LookupDiff("build-1", build.DiffType(storage.MemfileName)).Return(diff, true)

	src, err := ResolveSeekable(cache, "build-1", storage.MemfileName)
	require.NoError(t, err)

	size, err := src.Size(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1234), size)
}

func TestSeekableSource_Stream(t *testing.T) {
	t.Parallel()

	data := []byte("diff bytes")

	diff := buildmocks.NewMockDiff(t)
	diff.EXPECT().Slice(mock.Anything, int64(0), int64(len(data)), (*storage.FrameTable)(nil)).Return(data, nil)

	cache := peerservermocks.NewMockCache(t)
	cache.EXPECT().LookupDiff("build-1", build.DiffType(storage.MemfileName)).Return(diff, true)

	src, err := ResolveSeekable(cache, "build-1", storage.MemfileName)
	require.NoError(t, err)

	sender := &collectSender{}
	err = src.Stream(t.Context(), 0, int64(len(data)), sender)
	require.NoError(t, err)
	assert.Equal(t, data, sender.data)
}

// windowRecordingDiff serves synthetic data and records every Slice request so
// tests can assert the stream materializes bounded windows.
type windowRecordingDiff struct {
	size   int64
	slices []int64
}

func (d *windowRecordingDiff) Close() error { return nil }

func (d *windowRecordingDiff) ReadAt(context.Context, []byte, int64, *storage.FrameTable) (int, error) {
	panic("windowRecordingDiff.ReadAt must not be called by the peer stream")
}

func (d *windowRecordingDiff) Slice(_ context.Context, off, length int64, _ *storage.FrameTable) ([]byte, error) {
	d.slices = append(d.slices, length)

	return bytes.Repeat([]byte{byte(off % 251)}, int(length)), nil
}

func (d *windowRecordingDiff) CacheKey() build.DiffStoreKey { return build.DiffStoreKey("test") }

func (d *windowRecordingDiff) CachePath(context.Context) (string, error) { return "", nil }

func (d *windowRecordingDiff) Size(context.Context) (int64, error) { return d.size, nil }

func (d *windowRecordingDiff) FileSize(context.Context) (int64, error) { return d.size, nil }

func (d *windowRecordingDiff) BlockSize() int64 { return 4096 }

// A range larger than the window bound must be sliced window by window and
// reassemble at the receiver, with every message inside the transport bound
// (REQ-D7, audit §9.2).
func TestSeekableSource_StreamWindowsLargeRanges(t *testing.T) {
	t.Parallel()

	const rangeLength = peerStreamChunkSize*2 + 1024

	diff := &windowRecordingDiff{size: rangeLength}
	cache := peerservermocks.NewMockCache(t)
	cache.EXPECT().LookupDiff("build-1", build.DiffType(storage.MemfileName)).Return(diff, true)

	src, err := ResolveSeekable(cache, "build-1", storage.MemfileName)
	require.NoError(t, err)

	sender := &collectSender{}
	require.NoError(t, src.Stream(t.Context(), 0, rangeLength, sender))

	require.Len(t, sender.data, rangeLength)
	require.Equal(t, []int64{peerStreamChunkSize, peerStreamChunkSize, 1024}, diff.slices,
		"a large range must be materialized window by window")

	for i, n := range sender.sends {
		assert.LessOrEqual(t, n, sendChunkSize, "send %d exceeds the per-message bound", i)
	}
}
