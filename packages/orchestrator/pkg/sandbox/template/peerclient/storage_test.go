package peerclient

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	orchestratormocks "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator/mocks"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func TestPeerStorageProvider_OpenBlob_ExtractsFileName(t *testing.T) {
	t.Parallel()

	stream := orchestratormocks.NewMockChunkService_GetBuildBlobClient(t)
	stream.EXPECT().Recv().Return(&orchestrator.GetBuildBlobResponse{Data: []byte("data")}, nil).Once()
	stream.EXPECT().Recv().Return(nil, io.EOF).Once()

	client := orchestratormocks.NewMockChunkServiceClient(t)
	client.EXPECT().GetBuildBlob(mock.Anything, mock.MatchedBy(func(req *orchestrator.GetBuildBlobRequest) bool {
		return req.GetBuildId() == "build-1" && req.GetName() == "snapfile"
	})).Return(stream, nil)

	base := storage.NewMockStorageProvider(t)

	p := newPeerStorageProvider(base, client, &atomic.Bool{}, "peer-test:1234", nil)
	blob, err := p.OpenBlob(t.Context(), "build-1/snapfile")
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = blob.WriteTo(t.Context(), &buf)
	require.NoError(t, err)
	assert.Equal(t, "data", buf.String())
}

func TestPeerStorageProvider_OpenSeekable_ExtractsFileName(t *testing.T) {
	t.Parallel()

	client := orchestratormocks.NewMockChunkServiceClient(t)
	client.EXPECT().GetBuildFileSize(mock.Anything, mock.MatchedBy(func(req *orchestrator.GetBuildFileSizeRequest) bool {
		return req.GetBuildId() == "build-1" && req.GetName() == "memfile"
	})).Return(&orchestrator.GetBuildFileSizeResponse{TotalSize: 512}, nil)

	base := storage.NewMockStorageProvider(t)

	p := newPeerStorageProvider(base, client, &atomic.Bool{}, "peer-test:1234", nil)
	ff, err := p.OpenSeekable(t.Context(), "build-1/memfile")
	require.NoError(t, err)

	size, err := ff.Size(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(512), size)
}

// TestTryPeerDropsConnOnTransportError pins REQ-D7's connection hygiene: a peer
// that fails at the transport level retires the cached connection so the next
// resolve re-dials instead of reusing a dead one.
func TestTryPeerDropsConnOnTransportError(t *testing.T) {
	t.Parallel()

	client := orchestratormocks.NewMockChunkServiceClient(t)
	client.EXPECT().GetBuildFileSize(mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.Unavailable, "peer gone"))

	var dropped atomic.Bool

	h := peerHandle{
		client:   client,
		buildID:  "build-1",
		name:     storage.MemfileName,
		uploaded: &atomic.Bool{},
		dropConn: func() { dropped.Store(true) },
	}

	_, _ = tryPeer(t.Context(), &h, "size peer-seekable", func(ctx context.Context) (peerAttempt[int64], error) {
		resp, err := h.client.GetBuildFileSize(ctx, &orchestrator.GetBuildFileSizeRequest{BuildId: h.buildID, Name: h.name})
		if err != nil {
			return peerAttempt[int64]{}, err
		}

		return peerAttempt[int64]{value: resp.GetTotalSize(), hit: true}, nil
	})

	require.True(t, dropped.Load(), "a transport-level peer failure must retire the cached connection")
}

// TestTryPeerKeepsConnOnNotAvailable pins the other side: a peer that answers
// "not available" is an application-level miss and its connection must stay.
func TestTryPeerKeepsConnOnNotAvailable(t *testing.T) {
	t.Parallel()

	client := orchestratormocks.NewMockChunkServiceClient(t)
	client.EXPECT().GetBuildFileSize(mock.Anything, mock.Anything).
		Return(&orchestrator.GetBuildFileSizeResponse{Availability: &orchestrator.PeerAvailability{NotAvailable: true}}, nil)

	var dropped atomic.Bool

	h := peerHandle{
		client:   client,
		buildID:  "build-1",
		name:     storage.MemfileName,
		uploaded: &atomic.Bool{},
		dropConn: func() { dropped.Store(true) },
	}

	res, _ := tryPeer(t.Context(), &h, "size peer-seekable", func(ctx context.Context) (peerAttempt[int64], error) {
		resp, err := h.client.GetBuildFileSize(ctx, &orchestrator.GetBuildFileSizeRequest{BuildId: h.buildID, Name: h.name})
		if err != nil {
			return peerAttempt[int64]{}, err
		}

		if !checkPeerAvailability(resp.GetAvailability(), h.uploaded) {
			return peerAttempt[int64]{}, nil
		}

		return peerAttempt[int64]{value: resp.GetTotalSize(), hit: true}, nil
	})

	require.False(t, res.hit)
	require.False(t, dropped.Load(), "an application-level miss must not retire the connection")
}
