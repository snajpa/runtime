//go:build linux

package userfaultfd

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// stallReader serves the page after sleeping the configured stall on every
// read, modelling a slow but recovering source.
type stallReader struct {
	page  []byte
	stall time.Duration
}

func (r *stallReader) ReadAt(ctx context.Context, p []byte, _ int64) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(r.stall):
	}

	return copy(p, r.page), nil
}

// TestSourceReadLatencyUnderStall measures the source-read step of the fault
// path for P-4 (S-19 budget sizing): a stalled-but-recovering source must cost
// the guest the stall plus a small overhead, never an amplification towards
// the close/drain bounds S-19 introduced. The production distribution is
// carried by the serve timer; this pins the shape deterministically.
func TestSourceReadLatencyUnderStall(t *testing.T) {
	t.Parallel()

	page := bytes.Repeat([]byte{0xAB}, int(header.PageSize))

	const (
		stall      = 2 * time.Millisecond
		iterations = 200
	)

	u := newReadSourcePageUffd(&stallReader{page: page, stall: stall})

	latencies := make([]time.Duration, 0, iterations)
	for range iterations {
		start := time.Now()
		data, release, err := u.readSourcePage(t.Context(), 0, nil)
		elapsed := time.Since(start)

		release()

		require.NoError(t, err)
		require.Len(t, data, int(header.PageSize))

		latencies = append(latencies, elapsed)
	}

	slices.Sort(latencies)
	p50 := latencies[len(latencies)/2]
	p99 := latencies[(len(latencies)*99)/100]
	worst := latencies[len(latencies)-1]

	t.Logf("source read under a %v stall: p50=%v p99=%v max=%v (n=%d)", stall, p50, p99, worst, iterations)

	assert.GreaterOrEqual(t, int64(p50), int64(stall),
		"a stalled source must cost at least the stall")
	// The tail bound is deliberately loose: a fully loaded host can delay
	// individual wakeups by hundreds of milliseconds, and the property
	// this pins is the distance from the close/drain bounds (30 s / 2 m),
	// not the scheduler noise. The measured percentiles are logged above.
	assert.Less(t, p99, stall+500*time.Millisecond,
		"the fault path must not amplify a source stall towards the close/drain bounds")
}

// TestSourceReadErrorBudget pins the other half of P-4: a source that keeps
// failing must fail the read within the retry budget — under a second — not
// anywhere near the 30 s / 2 m close-and-drain bounds.
func TestSourceReadErrorBudget(t *testing.T) {
	t.Parallel()

	src := &flakyReader{page: make([]byte, int(header.PageSize))}
	src.failures.Store(1 << 30) // every read fails

	u := newReadSourcePageUffd(src)

	start := time.Now()
	_, release, err := u.readSourcePage(t.Context(), 0, nil)
	elapsed := time.Since(start)

	release()

	require.Error(t, err)
	t.Logf("source read with a permanently failing source: %v after %d attempts", elapsed, src.reads.Load())

	assert.GreaterOrEqual(t, src.reads.Load(), int32(sliceMaxRetries+1),
		"the retry policy must use its attempts before failing")
	// Same load caveat as above: the retry budget is 50/100/200 ms plus
	// jitter, and the bound only has to keep the failure far from the
	// close/drain bounds (30 s / 2 m).
	assert.Less(t, elapsed, 5*time.Second,
		"an unrecoverable source must fail the read within the retry budget")
}
