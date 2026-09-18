package metrics

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const orchestratorChunkSlice = "orchestrator.chunk.slice"

type Metrics struct {
	ChunkSliceTimerFactory telemetry.FloatTimerFactory

	// Fetch admission (S-18): how long fetches wait for a slot, how many run
	// and queue (backpressure depth), and how many waits their context cut
	// short.
	FetchAdmissionWait     metric.Float64Histogram
	FetchAdmissionInFlight metric.Int64UpDownCounter
	FetchAdmissionQueued   metric.Int64UpDownCounter
	FetchAdmissionExpired  metric.Int64Counter
}

func NewMetrics(meterProvider metric.MeterProvider) (Metrics, error) {
	var m Metrics

	blocksMeter := meterProvider.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/metrics")

	var err error
	if m.ChunkSliceTimerFactory, err = telemetry.NewFloatTimerFactory(
		blocksMeter, orchestratorChunkSlice,
		"Time taken by Chunker to serve a Slice() (source=mmap when served from cache)",
		"Bytes returned",
	); err != nil {
		return m, fmt.Errorf("error creating chunk slice timer factory: %w", err)
	}

	if m.FetchAdmissionWait, err = blocksMeter.Float64Histogram("orchestrator.chunk.fetch.wait",
		metric.WithDescription("Time a chunk fetch waited for an admission slot"),
		metric.WithUnit("s"),
	); err != nil {
		return m, fmt.Errorf("error creating fetch admission wait histogram: %w", err)
	}

	if m.FetchAdmissionInFlight, err = blocksMeter.Int64UpDownCounter("orchestrator.chunk.fetch.in_flight",
		metric.WithDescription("Chunk fetches currently holding an admission slot"),
	); err != nil {
		return m, fmt.Errorf("error creating fetch admission in-flight counter: %w", err)
	}

	if m.FetchAdmissionQueued, err = blocksMeter.Int64UpDownCounter("orchestrator.chunk.fetch.queued",
		metric.WithDescription("Chunk fetches waiting for an admission slot (backpressure depth)"),
	); err != nil {
		return m, fmt.Errorf("error creating fetch admission queued counter: %w", err)
	}

	if m.FetchAdmissionExpired, err = blocksMeter.Int64Counter("orchestrator.chunk.fetch.expired",
		metric.WithDescription("Chunk fetches whose admission wait was cut short by their context"),
	); err != nil {
		return m, fmt.Errorf("error creating fetch admission expired counter: %w", err)
	}

	return m, nil
}
