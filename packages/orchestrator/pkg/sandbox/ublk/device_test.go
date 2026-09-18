//go:build linux

package ublk

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOptionsNormalized(t *testing.T) {
	t.Parallel()

	t.Run("fills in unset fields", func(t *testing.T) {
		t.Parallel()

		got, err := Options{}.normalized()
		if err != nil {
			t.Fatalf("normalized: %v", err)
		}

		// Discard is not defaulted: the zero value means "do not advertise
		// discard", and callers that want the defaults start from
		// DefaultOptions.
		want := DefaultOptions()
		want.Discard = false

		if got != want {
			t.Errorf("normalized = %+v, want %+v", got, want)
		}
	})

	t.Run("keeps explicit values", func(t *testing.T) {
		t.Parallel()

		want := Options{Queues: 4, QueueDepth: 32, MaxIOBufBytes: 1 << 20, BlockSize: 512, Discard: false}

		got, err := want.normalized()
		if err != nil {
			t.Fatalf("normalized: %v", err)
		}
		if got != want {
			t.Errorf("normalized = %+v, want %+v", got, want)
		}
	})

	t.Run("rejects out of range values", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name string
			opts Options
		}{
			{"no queues", Options{Queues: -1}},
			{"too many queues", Options{Queues: ublkMaxQueueDepth + 1}},
			{"no depth", Options{QueueDepth: -1}},
			{"too deep", Options{QueueDepth: ublkMaxQueueDepth + 1}},
			{"tiny buffer", Options{MaxIOBufBytes: 1024}},
			{"huge buffer", Options{MaxIOBufBytes: maxIOBufBytes + 4096}},
			{"odd block size", Options{BlockSize: 1024}},
		} {
			if _, err := tc.opts.normalized(); err == nil {
				t.Errorf("%s: normalized accepted %+v", tc.name, tc.opts)
			}
		}
	})
}

// The commit result is the only place a backend failure can surface, so it has
// to carry the errno the kernel expects.
func TestErrnoResult(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want int32
	}{
		{"errno", unix.ENOSPC, -int32(unix.ENOSPC)},
		{"wrapped errno", fmt.Errorf("writing: %w", unix.EPERM), -int32(unix.EPERM)},
		{"plain error", errors.New("backend failed"), -int32(unix.EIO)},
		{"canceled", context.Canceled, -int32(unix.EINTR)},
		{"deadline", context.DeadlineExceeded, -int32(unix.ETIMEDOUT)},
	} {
		if got := errnoResult(tc.err); got != tc.want {
			t.Errorf("%s: errnoResult = %d, want %d", tc.name, got, tc.want)
		}
	}
}
