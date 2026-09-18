//go:build linux

package block

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// nodeFetchAdmission is the process-wide admission gate for upstream chunk
// fetches: every Chunker shares it, so a burst of guest faults cannot
// stampede the object store (REQ-D2). The limit is applied from the
// max-concurrent-chunk-fetches flag on each acquisition.
var nodeFetchAdmission = newFetchAdmission(int64(featureflags.MaxConcurrentChunkFetches.Fallback()))

// fetchAdmission is a FIFO admission gate: at most `limit` fetches run at
// once, the rest wait in arrival order, and every wait is bounded by the
// waiter's context.
type fetchAdmission struct {
	mu sync.Mutex

	limit    int64
	inFlight int64
	waiters  []chan struct{}
}

func newFetchAdmission(limit int64) *fetchAdmission {
	if limit < 1 {
		limit = 1
	}

	return &fetchAdmission{limit: limit}
}

// acquire reserves one fetch slot, waiting for a free one under ctx. The
// granted slot is already accounted in inFlight when acquire returns nil, so
// the caller only has to release it.
func (a *fetchAdmission) acquire(ctx context.Context, flags *featureflags.Client, m metrics.Metrics) error {
	a.applyLimit(ctx, flags)

	a.mu.Lock()
	if a.inFlight < a.limit {
		a.inFlight++
		a.mu.Unlock()

		m.FetchAdmissionInFlight.Add(ctx, 1)

		return nil
	}

	granted := make(chan struct{})
	a.waiters = append(a.waiters, granted)
	a.mu.Unlock()

	m.FetchAdmissionQueued.Add(ctx, 1)
	start := time.Now()

	select {
	case <-granted:
		m.FetchAdmissionQueued.Add(ctx, -1)
		m.FetchAdmissionWait.Record(ctx, time.Since(start).Seconds())
		m.FetchAdmissionInFlight.Add(ctx, 1)

		return nil
	case <-ctx.Done():
		m.FetchAdmissionQueued.Add(ctx, -1)
		m.FetchAdmissionWait.Record(ctx, time.Since(start).Seconds())
		m.FetchAdmissionExpired.Add(ctx, 1)

		a.mu.Lock()
		for i, waiter := range a.waiters {
			if waiter == granted {
				// Still queued: leave the queue and report the context error.
				a.waiters = append(a.waiters[:i], a.waiters[i+1:]...)
				a.mu.Unlock()

				return ctx.Err()
			}
		}
		// Granted concurrently with the cancellation: pass the slot on to the
		// next waiter instead of leaking it.
		a.grantNextLocked()
		a.mu.Unlock()

		return ctx.Err()
	}
}

func (a *fetchAdmission) release(ctx context.Context, m metrics.Metrics) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.inFlight > 0 {
		a.inFlight--
	}

	m.FetchAdmissionInFlight.Add(ctx, -1)

	a.grantNextLocked()
}

// grantNextLocked hands free slots to the oldest waiters; the caller holds mu.
func (a *fetchAdmission) grantNextLocked() {
	for a.inFlight < a.limit && len(a.waiters) > 0 {
		next := a.waiters[0]
		a.waiters = a.waiters[1:]
		a.inFlight++
		close(next)
	}
}

// applyLimit resizes the gate to the current flag value, clamped to at least
// one; growing the limit admits queued waiters immediately.
func (a *fetchAdmission) applyLimit(ctx context.Context, flags *featureflags.Client) {
	if flags == nil {
		return
	}

	limit := int64(flags.IntFlag(ctx, featureflags.MaxConcurrentChunkFetches))
	if limit < 1 {
		logger.L().Warn(ctx, "fetch admission limit is too low, falling back",
			zap.Int64("limit", limit))

		limit = int64(featureflags.MaxConcurrentChunkFetches.Fallback())
	}

	a.setLimit(limit)
}

func (a *fetchAdmission) setLimit(limit int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if limit < 1 || a.limit == limit {
		return
	}

	a.limit = limit
	a.grantNextLocked()
}
