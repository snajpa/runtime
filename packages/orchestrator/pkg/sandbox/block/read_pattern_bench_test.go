//go:build linux

package block

import (
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// The steady-state readahead observer (S-23, slice 1) runs in Chunker.Slice's
// miss path, so its own cost is paid on every non-cached read. These
// benchmarks measure that cost through a manual-reader SDK provider — the real
// instrumentation path (attribute set plus counter/histogram calls), not the
// no-op global meter — because that is what a production process pays.
//
// Sequential is the common case: a miss that continues the previous run, and
// the only case that touches the three opportunity instruments. Gap measures
// the reset path (locking and bookkeeping only). Parallel puts several readers
// on one chunker, the shared-chunker case the design note records as a caveat.

const benchChunkLen = 4096

// swapReadaheadInstruments points the package instruments at a meter owned by
// a manual-reader provider for the duration of one benchmark.
func swapReadaheadInstruments(tb testing.TB) {
	tb.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := mp.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block")

	prevHistogram, prevCounter, prevBytes := readaheadRunHistogram, readaheadOpportunityCounter, readaheadOpportunityBytes

	readaheadRunHistogram = utils.Must(m.Int64Histogram("orchestrator.block.readahead.sequential_miss_run"))
	readaheadOpportunityCounter = utils.Must(m.Int64Counter("orchestrator.block.readahead.opportunity"))
	readaheadOpportunityBytes = utils.Must(m.Int64Counter("orchestrator.block.readahead.opportunity_bytes"))

	tb.Cleanup(func() {
		readaheadRunHistogram, readaheadOpportunityCounter, readaheadOpportunityBytes = prevHistogram, prevCounter, prevBytes
	})
}

func BenchmarkReadPatternObserveSequential(b *testing.B) {
	swapReadaheadInstruments(b)

	var p readPattern

	ctx := b.Context()
	off := int64(0)

	p.observe(ctx, off, benchChunkLen, storage.RootFSObjectType) // starts the run

	for b.Loop() {
		off += benchChunkLen
		p.observe(ctx, off, benchChunkLen, storage.RootFSObjectType)
	}
}

func BenchmarkReadPatternObserveGap(b *testing.B) {
	swapReadaheadInstruments(b)

	var p readPattern

	ctx := b.Context()

	for b.Loop() {
		p.observe(ctx, 1<<40, benchChunkLen, storage.RootFSObjectType)
	}
}

func BenchmarkReadPatternObserveParallel(b *testing.B) {
	swapReadaheadInstruments(b)

	var p readPattern

	ctx := b.Context()

	b.RunParallel(func(pb *testing.PB) {
		off := int64(0)
		for pb.Next() {
			off += benchChunkLen
			p.observe(ctx, off, benchChunkLen, storage.RootFSObjectType)
		}
	})
}
