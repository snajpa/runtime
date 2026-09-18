//go:build linux

package block

import (
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

func TestReadPatternObserve(t *testing.T) {
	t.Parallel()

	var p readPattern
	ctx := t.Context()

	seq, run := p.observe(ctx, 0, 100, storage.MemfileObjectType)
	require.False(t, seq, "the first miss starts a run")
	require.EqualValues(t, 1, run)

	seq, run = p.observe(ctx, 100, 100, storage.MemfileObjectType)
	require.True(t, seq, "the next chunk continues the run")
	require.EqualValues(t, 2, run)

	seq, run = p.observe(ctx, 200, 50, storage.MemfileObjectType)
	require.True(t, seq, "continuation uses the previous chunk's length")
	require.EqualValues(t, 3, run)

	seq, run = p.observe(ctx, 500, 100, storage.MemfileObjectType)
	require.False(t, seq, "a gap starts a new run")
	require.EqualValues(t, 1, run)

	seq, run = p.observe(ctx, 500, 100, storage.MemfileObjectType)
	require.False(t, seq, "a repeated offset is not a continuation")
	require.EqualValues(t, 1, run)

	seq, run = p.observe(ctx, 0, 100, storage.MemfileObjectType)
	require.False(t, seq, "a backward read starts a new run")
	require.EqualValues(t, 1, run)
}

// TestReadPatternMetrics pins the observer's signals: only continuations count
// as lookahead opportunities, and the run histogram records the run length.
//
//nolint:paralleltest // swaps the package-level readahead instruments
func TestReadPatternMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := mp.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block")

	prevHistogram, prevCounter, prevBytes := readaheadRunHistogram, readaheadOpportunityCounter, readaheadOpportunityBytes
	readaheadRunHistogram = utils.Must(m.Int64Histogram("orchestrator.block.readahead.sequential_miss_run"))
	readaheadOpportunityCounter = utils.Must(m.Int64Counter("orchestrator.block.readahead.opportunity"))
	readaheadOpportunityBytes = utils.Must(m.Int64Counter("orchestrator.block.readahead.opportunity_bytes"))
	t.Cleanup(func() {
		readaheadRunHistogram, readaheadOpportunityCounter, readaheadOpportunityBytes = prevHistogram, prevCounter, prevBytes
	})

	var p readPattern
	ctx := t.Context()

	p.observe(ctx, 0, 4096, storage.RootFSObjectType)      // run start: no opportunity
	p.observe(ctx, 4096, 4096, storage.RootFSObjectType)   // opportunity, run 2
	p.observe(ctx, 8192, 8192, storage.RootFSObjectType)   // opportunity, run 3
	p.observe(ctx, 100000, 4096, storage.RootFSObjectType) // reset: no opportunity

	require.EqualValues(t, 2, readaheadCounterValue(t, reader, "orchestrator.block.readahead.opportunity"))
	require.EqualValues(t, 12288, readaheadCounterValue(t, reader, "orchestrator.block.readahead.opportunity_bytes"))

	count, sum := readaheadHistogramValue(t, reader, "orchestrator.block.readahead.sequential_miss_run")
	require.EqualValues(t, 2, count, "one sample per continuation")
	require.EqualValues(t, 5, sum, "runs 2 + 3")
}

func readaheadCounterValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s must be an int64 sum", name)

			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}

			return total
		}
	}

	return 0
}

func readaheadHistogramValue(t *testing.T, reader *sdkmetric.ManualReader, name string) (uint64, int64) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			hist, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok, "%s must be an int64 histogram", name)

			var count uint64
			var sum int64
			for _, dp := range hist.DataPoints {
				count += dp.Count
				sum += dp.Sum
			}

			return count, sum
		}
	}

	return 0, 0
}
