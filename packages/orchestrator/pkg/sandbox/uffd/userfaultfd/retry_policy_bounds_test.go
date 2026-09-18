//go:build linux

package userfaultfd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSourceReadRetryPolicyBounds pins the retry policy readSourcePage's
// documentation states (userfaultfd.go): sliceMaxRetries attempts with
// exponential backoff, whose worst case stays within the documented "~2s"
// that must never block the REMOVE batch.
func TestSourceReadRetryPolicyBounds(t *testing.T) {
	t.Parallel()

	require.Equal(t, 3, sliceMaxRetries)
	require.Equal(t, 50*time.Millisecond, sliceRetryBaseDelay)
	require.Equal(t, 500*time.Millisecond, sliceRetryMaxDelay)

	// Worst-case total backoff for the retries: base, doubling each time,
	// capped at sliceRetryMaxDelay.
	var worstCase time.Duration
	delay := sliceRetryBaseDelay
	for range sliceMaxRetries {
		if delay > sliceRetryMaxDelay {
			delay = sliceRetryMaxDelay
		}
		worstCase += delay
		delay *= 2
	}

	assert.LessOrEqual(t, worstCase, 2*time.Second,
		"documented bound: up to ~2s of backoff must never block the REMOVE batch")
}
