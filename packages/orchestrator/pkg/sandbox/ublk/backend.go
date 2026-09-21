//go:build linux

package ublk

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"
)

// Backend is the storage a ublk device serves to the kernel: the part of the
// orchestrator's block.Device that the data plane needs.
type Backend interface {
	// ReadAt fills p with the bytes at off. It has to return an error unless
	// it filled p completely.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	// WriteAt writes p at off. It has to return an error unless it wrote all
	// of p.
	WriteAt(p []byte, off int64) (int, error)
	// WriteZeroesAt zeroes [off, off+length).
	WriteZeroesAt(off, length int64) (int, error)
	// Size is the backend size in bytes.
	Size(ctx context.Context) (int64, error)
	// BlockSize is the backend's block size.
	BlockSize() int64
}

// errnoResult converts a backend error into the negative errno a commit
// carries. Anything the backend does not tag with an errno becomes -EIO, and
// a canceled context becomes -EINTR: both surface as the request's error.
func errnoResult(err error) int32 {
	if errno, ok := errors.AsType[unix.Errno](err); ok {
		return -int32(errno)
	}

	switch {
	case errors.Is(err, context.Canceled):
		return -int32(unix.EINTR)
	case errors.Is(err, context.DeadlineExceeded):
		return -int32(unix.ETIMEDOUT)
	default:
		return -int32(unix.EIO)
	}
}
