package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The temp-residue counter is the monitoring hook for same-directory temp files
// whose cleanup failed: unlike upload.residue, nothing bounds them — no
// provider garbage collection sees them — so they must stay visible even though
// the writers that remove them cannot take a context.
//
//nolint:paralleltest // swaps the package-level tempResidue counter
func TestRemoveTempFileCountsResidue(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prevCounter := tempResidueCounter
	tempResidueCounter = newTempResidueCounter(mp.Meter("github.com/e2b-dev/infra/packages/shared/pkg/storage"))
	t.Cleanup(func() { tempResidueCounter = prevCounter })

	prevCount := tempResidue.Swap(0)
	t.Cleanup(func() { tempResidue.Store(prevCount) })

	// A removal that succeeds — or finds the file already gone — leaves no
	// residue behind.
	dir := t.TempDir()
	removeTempFile(filepath.Join(dir, "already-gone"))
	require.Zero(t, tempResidue.Load(), "a vanished temp file is not residue")

	// A removal that fails counts one file. A non-empty directory is a portable
	// way to make os.Remove fail.
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "child"), []byte("x"), 0o600))

	removeTempFile(blocked)

	require.EqualValues(t, 1, tempResidue.Load(), "a failed removal is one residue file")
	require.EqualValues(t, 1, tempResidueValue(t, reader), "the series reports the residue count")
}

// tempResidueValue reads the current value of the observable counter through a
// manual reader, mirroring the way the metric is collected in production.
func tempResidueValue(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "orchestrator.upload.temp_residue" {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "temp_residue must be an int64 sum")

			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}

			return total
		}
	}

	return 0
}
