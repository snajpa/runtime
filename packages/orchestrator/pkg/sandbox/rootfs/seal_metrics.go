//go:build linux

package rootfs

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// The background-seal state machine: SwapForBackgroundSeal freezes the current
// writable cache and hands it to the seal path, FoldSealed folds it back. These
// metrics make seal saturation and per-outcome failures visible (REQ-I1); tests
// swap the package-level instruments for a manual reader, the same way the
// block package swaps its counters.
var sealMeter = otel.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs")

const (
	sealSwapMetricName         = "orchestrator.sandbox.rootfs.seal_swap"
	sealFoldMetricName         = "orchestrator.sandbox.rootfs.seal_fold"
	sealFoldDurationMetricName = "orchestrator.sandbox.rootfs.seal_fold.duration"

	sealOutcomeAttribute = "outcome"
	sealOutcomeOK        = "ok"
	sealOutcomeError     = "error"
)

var (
	sealSwapCounter = utils.Must(sealMeter.Int64Counter(sealSwapMetricName,
		metric.WithDescription("Background seal swaps, by outcome; a swap hands the writable cache to the seal path."),
		metric.WithUnit("{swap}")))

	sealFoldCounter = utils.Must(sealMeter.Int64Counter(sealFoldMetricName,
		metric.WithDescription("Background seal folds, by outcome; a fold returns the sealed cache to the writable one."),
		metric.WithUnit("{fold}")))

	sealFoldDuration = utils.Must(sealMeter.Float64Histogram(sealFoldDurationMetricName,
		metric.WithDescription("Time to fold a sealing cache back into the writable cache."),
		metric.WithUnit("ms")))
)

func sealOutcome(err error) string {
	if err != nil {
		return sealOutcomeError
	}

	return sealOutcomeOK
}

func sealAttributes(err error) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(sealOutcomeAttribute, sealOutcome(err)))
}

// recordSealSwap counts one swap attempt.
func recordSealSwap(ctx context.Context, err error) {
	sealSwapCounter.Add(ctx, 1, sealAttributes(err))
}

// recordSealFold counts one fold attempt and records how long it took.
func recordSealFold(ctx context.Context, dur time.Duration, err error) {
	sealFoldCounter.Add(ctx, 1, sealAttributes(err))
	sealFoldDuration.Record(ctx, float64(dur.Milliseconds()), sealAttributes(err))
}
