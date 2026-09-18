//go:build linux

package block

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestPunchHoleFallsBackAndCountsWhenMadviseFails pins the fallback signal: a
// kernel or filesystem without MADV_REMOVE support still zero-fills the range,
// and the condition is counted instead of staying silent (audit §8.2).
//
//nolint:paralleltest // swaps the package-level madvise seam and fallback counter
func TestPunchHoleFallsBackAndCountsWhenMadviseFails(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prevCounter := madviseRemoveFallbackCounter
	madviseRemoveFallbackCounter = utils.Must(mp.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block").
		Int64ObservableCounter("orchestrator.block.cache.madv_remove_fallback",
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
				o.Observe(madviseRemoveFallbacks.Load())

				return nil
			})))
	t.Cleanup(func() { madviseRemoveFallbackCounter = prevCounter })

	prevFallbacks := madviseRemoveFallbacks.Swap(0)
	t.Cleanup(func() { madviseRemoveFallbacks.Store(prevFallbacks) })

	prevMadvise := madviseRemove
	madviseRemove = func([]byte, int) error { return unix.EINVAL }
	t.Cleanup(func() { madviseRemove = prevMadvise })

	page := int64(header.PageSize)
	cache, err := NewCache(page, page, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	data := bytes.Repeat([]byte{0xAB}, int(page))
	_, err = cache.WriteAt(data, 0)
	require.NoError(t, err)

	cache.punchHole(0, page)

	got, err := cache.sliceDirect(0, page)
	require.NoError(t, err)
	require.Equal(t, make([]byte, page), got, "the fallback must still zero-fill the range")
	require.EqualValues(t, 1, madviseFallbackValue(t, reader))

	// A punch the kernel accepts must not be counted: simulate support with a
	// no-op seam so the assertion does not depend on the test filesystem.
	madviseRemove = func([]byte, int) error { return nil }
	_, err = cache.WriteAt(data, 0)
	require.NoError(t, err)
	cache.punchHole(0, page)
	require.EqualValues(t, 1, madviseFallbackValue(t, reader), "a successful punch must not count")
}

func madviseFallbackValue(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "orchestrator.block.cache.madv_remove_fallback" {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "the fallback counter must be an int64 sum")

			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}

			return total
		}
	}

	return 0
}
