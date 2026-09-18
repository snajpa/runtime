//go:build linux

package peerserver

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
)

var _ SeekableSource = &seekableSource{}

// peerStreamChunkSize bounds how much of a requested range is materialized at
// once: the range is sliced and sent window by window, so serving a large
// range never allocates O(length) (REQ-D7, audit §9.2).
const peerStreamChunkSize = 4 << 20 // 4 MiB

// seekableSource serves seekable diff files (memfile, rootfs.ext4).
// Supports Size and random-access streaming via offset/length.
type seekableSource struct {
	diff build.Diff
}

func (f *seekableSource) Size(ctx context.Context) (int64, error) {
	return f.diff.Size(ctx)
}

func (f *seekableSource) Exists(_ context.Context) (bool, error) {
	return false, ErrNotSupported
}

func (f *seekableSource) Stream(ctx context.Context, offset, length int64, sender Sender) error {
	ctx, span := tracer.Start(ctx, "stream-seekable-file", trace.WithAttributes(
		attribute.Int64("offset", offset),
		attribute.Int64("length", length),
	))
	defer span.End()

	// P2P always serves uncompressed bytes — pass nil FrameTable. The range
	// is sliced window by window and each window is sent before the next is
	// read, so memory stays O(window) instead of O(length).
	for sent := int64(0); sent < length; {
		window := min(length-sent, peerStreamChunkSize)

		data, err := f.diff.Slice(ctx, offset+sent, window, nil)
		if err != nil {
			span.RecordError(err)

			return fmt.Errorf("slice diff at offset %d: %w", offset+sent, err)
		}
		if len(data) == 0 {
			// No progress: stop rather than spin on a source that returned
			// nothing for a non-empty window.
			break
		}

		if err := sendChunked(sender, data); err != nil {
			span.RecordError(err)

			return fmt.Errorf("send diff chunk: %w", err)
		}

		sent += int64(len(data))
	}

	return nil
}
