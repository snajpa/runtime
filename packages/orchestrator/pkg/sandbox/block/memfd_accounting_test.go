//go:build linux

package block

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestDedupedMemfdCache_HeldBytesAccounted pins the hold accounting: adopting a
// memfd for provisional serving adds its mapped size, and every release path
// subtracts exactly once — a second release (or a release without a hold) must
// not move the counter again.
//
//nolint:paralleltest // swaps the package-level memfdHeldBytes counter
func TestDedupedMemfdCache_HeldBytesAccounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prev := memfdHeldBytes
	memfdHeldBytes = utils.Must(mp.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block").
		Int64UpDownCounter("orchestrator.memfd.held_bytes"))
	t.Cleanup(func() { memfdHeldBytes = prev })

	ps := int64(header.PageSize)
	memfd, _ := newTestMemfd(t, ps*4)

	d := &DedupedMemfdCache{inflight: true, done: utils.NewSetOnce[*Cache]()}

	d.adoptMemfd(t.Context(), memfd, buildPackedIndex(roaring.New()))
	require.Equal(t, ps*4, heldBytesValue(t, reader), "the adopted memfd's mapped size is accounted")

	require.NoError(t, d.releaseMemfd(t.Context()))
	require.Zero(t, heldBytesValue(t, reader), "the release gives the bytes back")

	// Idempotent: a second release must not subtract again.
	require.NoError(t, d.releaseMemfd(t.Context()))
	require.Zero(t, heldBytesValue(t, reader), "a double release does not double-count")
}

// heldBytesValue reads the current value of the named int64 up/down counter
// from a manual reader.
func heldBytesValue(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "orchestrator.memfd.held_bytes" {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "held_bytes must be an int64 sum")

			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}

			return total
		}
	}

	t.Fatal("held_bytes metric not found in collected metrics")

	return 0
}
