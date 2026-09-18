//go:build linux

package block

import (
	"errors"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestDedupedMemfdCache_GraceReleasesWithoutSwap pins the grace backstop: when
// the provisional→deduped swap never happens (MarkSwapped is never called), the
// drain still releases the memfd within memfdSwapGrace, provisional reads then
// fail with BytesNotAvailableError, and the grace counter records the expiry.
//
//nolint:paralleltest // swaps the package-level grace and grace counter
func TestDedupedMemfdCache_GraceReleasesWithoutSwap(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prevCounter := swapGraceElapsedCounter
	swapGraceElapsedCounter = utils.Must(mp.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block").
		Int64Counter("orchestrator.memfd.swap_grace_elapsed"))
	t.Cleanup(func() { swapGraceElapsedCounter = prevCounter })

	prevGrace := memfdSwapGrace
	memfdSwapGrace = 50 * time.Millisecond
	t.Cleanup(func() { memfdSwapGrace = prevGrace })

	ps := int64(header.PageSize)
	const numPages = 64
	size := ps * numPages

	memfd, _ := newTestMemfd(t, size)
	base := &fakeOriginalDevice{data: make([]byte, size)}
	dirty := roaring.New()
	dirty.AddRange(0, numPages)

	metaOut := utils.NewSetOnce[*header.DiffMetadata]()
	cache, err := NewCacheFromMemfdDeduped(
		t.Context(), base, ps, t.TempDir()+"/dedup-grace", memfd, dirty,
		false, false, DedupBudget{}, nil, metaOut, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cache.Close() })

	// The drain completes and the memfd is held for the pending swap...
	_, err = cache.Wait(t.Context())
	require.NoError(t, err)

	// ...but MarkSwapped is never called, so the grace must release it.
	require.Eventually(t, func() bool {
		var bna BytesNotAvailableError
		_, serveErr := cache.ServeMemfd(make([]byte, ps), 0)

		return errors.As(serveErr, &bna)
	}, 10*time.Second, 5*time.Millisecond, "the memfd must be released within the grace")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Equal(t, int64(1), metricTotal(t, rm, "orchestrator.memfd.swap_grace_elapsed"),
		"the grace expiry must be recorded once")
}

// metricTotal sums a named int64 counter's datapoints.
func metricTotal(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %q must be an int64 sum", name)

			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}

			return total
		}
	}

	t.Fatalf("metric %q not found in collected metrics", name)

	return 0
}
