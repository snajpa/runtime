package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/lock"
)

func TestWritebackQueueBoundsAndDrops(t *testing.T) {
	t.Parallel()

	q := newWritebackQueue(1)

	release := make(chan struct{})
	started := make(chan struct{})

	require.True(t, q.submit(t.Context(), nil, func(context.Context) {
		close(started)
		<-release
	}))
	<-started

	// The only slot is busy: the next fill is dropped, not queued.
	require.False(t, q.submit(t.Context(), nil, func(context.Context) {}))

	// Once the fill finishes, the slot is reusable.
	close(release)
	require.Eventually(t, func() bool {
		return q.submit(t.Context(), nil, func(context.Context) {})
	}, time.Second, time.Millisecond)

	require.NoError(t, q.drain(t.Context()))
}

func TestWritebackQueueDrainsAndRejects(t *testing.T) {
	t.Parallel()

	q := newWritebackQueue(4)

	release := make(chan struct{})
	started := make(chan struct{})

	require.True(t, q.submit(t.Context(), nil, func(context.Context) {
		close(started)
		<-release
	}))
	<-started

	// A deadline shorter than the in-flight fill reports the deadline.
	deadlineCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, q.drain(deadlineCtx), context.DeadlineExceeded)

	// Once draining, new fills are refused.
	require.False(t, q.submit(t.Context(), nil, func(context.Context) {}))

	// The in-flight fill finishing lets the drain complete.
	close(release)
	require.NoError(t, q.drain(t.Context()))
}

func TestWritebackQueueTracksInstance(t *testing.T) {
	t.Parallel()

	q := newWritebackQueue(1)

	var wg sync.WaitGroup
	released := make(chan struct{})
	require.True(t, q.submit(t.Context(), &wg, func(context.Context) {
		<-released
	}))

	close(released)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the instance tracker was not released after the fill finished")
	}

	require.NoError(t, q.drain(t.Context()))
}

func TestRetryContendedLock(t *testing.T) {
	t.Parallel()

	t.Run("retries until the lock frees", func(t *testing.T) {
		t.Parallel()

		var attempts int
		value, err := retryContendedLock(t.Context(), 3, time.Millisecond, func() (int, error) {
			attempts++
			if attempts < 3 {
				return 0, fmt.Errorf("cache lock: %w", lock.ErrLockAlreadyHeld)
			}

			return attempts, nil
		})
		require.NoError(t, err)
		require.Equal(t, 3, value)
		require.Equal(t, 3, attempts)
	})

	t.Run("gives up after the attempt budget", func(t *testing.T) {
		t.Parallel()

		var attempts int
		_, err := retryContendedLock(t.Context(), 3, time.Millisecond, func() (int, error) {
			attempts++

			return 0, lock.ErrLockAlreadyHeld
		})
		require.ErrorIs(t, err, lock.ErrLockAlreadyHeld)
		require.Equal(t, 3, attempts)
	})

	t.Run("returns other errors immediately", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("boom")
		var attempts int
		_, err := retryContendedLock(t.Context(), 3, time.Millisecond, func() (int, error) {
			attempts++

			return 0, boom
		})
		require.ErrorIs(t, err, boom)
		require.Equal(t, 1, attempts)
	})

	t.Run("stops when the context is cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := retryContendedLock(ctx, 3, time.Millisecond, func() (int, error) {
			return 0, lock.ErrLockAlreadyHeld
		})
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, lock.ErrLockAlreadyHeld)
	})
}
