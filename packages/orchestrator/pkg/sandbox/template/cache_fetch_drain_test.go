//go:build linux

package template

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newFetchTestCache() *Cache {
	return &Cache{}
}

// TestCacheFetchDrain pins S-19: fetches are tracked on the cache's own ctx,
// Stop cancels them, and the drain is bounded.
//
//nolint:paralleltest // mutates the package-level drain bound.
func TestCacheFetchDrain(t *testing.T) {
	prev := templateFetchDrainTimeout
	templateFetchDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { templateFetchDrainTimeout = prev })

	c := newFetchTestCache()

	var ran atomic.Bool

	started := make(chan struct{})
	require.True(t, c.startFetch(t.Context(), func(ctx context.Context) {
		ran.Store(true)
		close(started)
		<-ctx.Done()
	}))

	<-started
	require.True(t, ran.Load())

	require.NoError(t, c.stopFetches(t.Context()), "a ctx-aware fetch must drain on cancel")

	// A stopped cache must not start new background work.
	require.False(t, c.startFetch(t.Context(), func(context.Context) {}))
}

// TestCacheFetchDrainBounded pins S-19: a fetch that ignores cancellation must
// not hang shutdown beyond the drain bound.
//
//nolint:paralleltest // mutates the package-level drain bound.
func TestCacheFetchDrainBounded(t *testing.T) {
	prev := templateFetchDrainTimeout
	templateFetchDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { templateFetchDrainTimeout = prev })

	c := newFetchTestCache()

	block := make(chan struct{})
	defer close(block)

	require.True(t, c.startFetch(t.Context(), func(context.Context) {
		<-block
	}))

	start := time.Now()
	err := c.stopFetches(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second, "the drain must be bounded")
}
