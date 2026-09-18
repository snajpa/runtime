//go:build linux

package rootfs

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestSealMetrics pins the background-seal signals (REQ-I1): swap and fold
// outcomes are counted, and the fold duration is recorded.
//
//nolint:paralleltest // swaps the package-level seal instruments
func TestSealMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := mp.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs")

	prevSwap, prevFold, prevDuration := sealSwapCounter, sealFoldCounter, sealFoldDuration
	sealSwapCounter = utils.Must(m.Int64Counter(sealSwapMetricName))
	sealFoldCounter = utils.Must(m.Int64Counter(sealFoldMetricName))
	sealFoldDuration = utils.Must(m.Float64Histogram(sealFoldDurationMetricName))
	t.Cleanup(func() {
		sealSwapCounter, sealFoldCounter, sealFoldDuration = prevSwap, prevFold, prevDuration
	})

	recordSealSwap(t.Context(), nil)
	recordSealSwap(t.Context(), errors.New("swap failed"))
	recordSealFold(t.Context(), 1500*time.Millisecond, nil)

	require.Equal(t, map[string]int64{"ok": 1, "error": 1}, sealCounterByOutcome(t, reader, sealSwapMetricName))
	require.Equal(t, map[string]int64{"ok": 1}, sealCounterByOutcome(t, reader, sealFoldMetricName))

	count, sum := sealFoldHistogram(t, reader, sealFoldDurationMetricName)
	require.EqualValues(t, 1, count)
	require.InDelta(t, 1500.0, sum, 0.001, "the fold must be recorded in milliseconds")
}

func sealCounterByOutcome(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	outcomes := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s must be an int64 sum", name)

			for _, dp := range sum.DataPoints {
				outcome, _ := dp.Attributes.Value(sealOutcomeAttribute)
				outcomes[outcome.AsString()] += dp.Value
			}
		}
	}

	return outcomes
}

func sealFoldHistogram(t *testing.T, reader *sdkmetric.ManualReader, name string) (uint64, float64) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "%s must be a float64 histogram", name)

			var count uint64
			var sum float64
			for _, dp := range hist.DataPoints {
				count += dp.Count
				sum += dp.Sum
			}

			return count, sum
		}
	}

	return 0, 0
}
