//go:build linux

package build

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCanRetryCacheClosed pins S-19: the eviction re-plan loop is bounded and
// cancellable.
//
//nolint:paralleltest // mutates the package-level retry delay.
func TestCanRetryCacheClosed(t *testing.T) {
	prev := cacheClosedRetryDelay
	cacheClosedRetryDelay = time.Millisecond
	t.Cleanup(func() { cacheClosedRetryDelay = prev })

	ctx := t.Context()

	for attempt := range maxCacheClosedRetries {
		require.NoError(t, canRetryCacheClosed(ctx, attempt), "attempt %d must be allowed", attempt)
	}

	require.Error(t, canRetryCacheClosed(ctx, maxCacheClosedRetries), "the cap must stop the loop")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	require.ErrorIs(t, canRetryCacheClosed(cancelled, 0), context.Canceled)
}
