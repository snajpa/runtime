//go:build linux

package block

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// Steady-state readahead (S-23, REQ-D6) is a two-slice design; this file is
// slice one, the observer. It classifies consecutive non-cached reads by chunk
// continuity and reports the run length and the bytes a lookahead window could
// have fetched ahead. Nothing is fetched here, so the observer cannot change
// I/O behaviour; slice two (admission-gated lookahead issuance) lands with
// benchmark evidence once this telemetry shows the opportunity.
var (
	readaheadRunHistogram = utils.Must(meter.Int64Histogram(
		"orchestrator.block.readahead.sequential_miss_run",
		metric.WithDescription("Length of a run of chunk-continuous cache misses, in chunks; the shape a steady-state lookahead policy would feed."),
		metric.WithUnit("{chunk}")))

	readaheadOpportunityCounter = utils.Must(meter.Int64Counter(
		"orchestrator.block.readahead.opportunity",
		metric.WithDescription("Cache misses that immediately continue the previous miss's chunk — each is a lookahead opportunity."),
		metric.WithUnit("{miss}")))

	readaheadOpportunityBytes = utils.Must(meter.Int64Counter(
		"orchestrator.block.readahead.opportunity_bytes",
		metric.WithDescription("Bytes of those misses; what a lookahead window could fetch ahead."),
		metric.WithUnit("By")))
)

// readaheadMemfileAttrs/readaheadRootFSAttrs cache one attribute-set option per
// object type: the hook runs on every cache miss, so the option is built once
// at init rather than per call (the pattern memfd.go uses for its per-page
// counter). Types outside the two a chunker serves fall back to building the
// option on the fly.
var (
	readaheadMemfileAttrs = metric.WithAttributeSet(attribute.NewSet(attribute.String("obj_type", storage.MemfileObjectType.String())))
	readaheadRootFSAttrs  = metric.WithAttributeSet(attribute.NewSet(attribute.String("obj_type", storage.RootFSObjectType.String())))
)

func readaheadAttrs(objType storage.SeekableObjectType) metric.MeasurementOption {
	switch objType {
	case storage.MemfileObjectType:
		return readaheadMemfileAttrs
	case storage.RootFSObjectType:
		return readaheadRootFSAttrs
	default:
		return metric.WithAttributeSet(attribute.NewSet(attribute.String("obj_type", objType.String())))
	}
}

// readPattern is the per-chunker steady-state observer. Its state is two
// integers plus the current run length: a bounded-memory alternative to the
// per-block map the older PrefetchTracker kept (its entry map grew with every
// touched block, audit §8.1). A request only continues a run when it starts
// exactly at the end of the previous miss's chunk; gaps, backward reads and
// duplicates start a new run.
type readPattern struct {
	mu sync.Mutex

	lastChunkOff int64
	lastChunkLen int64
	runChunks    int64
}

// observe records one non-cached read whose first chunk is
// [chunkOff, chunkOff+chunkLen) and reports whether it continues the previous
// miss plus the run length in chunks.
func (p *readPattern) observe(ctx context.Context, chunkOff, chunkLen int64, objType storage.SeekableObjectType) (bool, int64) {
	p.mu.Lock()

	sequential := p.runChunks > 0 && chunkOff == p.lastChunkOff+p.lastChunkLen
	if sequential {
		p.runChunks++
	} else {
		p.runChunks = 1
	}

	p.lastChunkOff = chunkOff
	p.lastChunkLen = chunkLen
	run := p.runChunks
	p.mu.Unlock()

	if !sequential {
		return false, run
	}

	attrs := readaheadAttrs(objType)
	readaheadOpportunityCounter.Add(ctx, 1, attrs)
	readaheadOpportunityBytes.Add(ctx, chunkLen, attrs)
	readaheadRunHistogram.Record(ctx, run, attrs)

	return true, run
}
