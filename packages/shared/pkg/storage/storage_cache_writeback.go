package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/lock"
)

const (
	// maxConcurrentWritebacks bounds the cache fills running at once across
	// the process. A submission with no free slot is dropped and counted
	// instead of queued, so neither goroutines nor memory grow with load
	// (REQ-C4); the cached data was already served to the caller, so a drop
	// costs a future cache miss, never data.
	maxConcurrentWritebacks = 64
	// writebackMaxAttempts is the attempt budget for a cache fill that keeps
	// losing the cache lock to another writer. Contention is normal cache
	// dedup, but a writer that dies mid-fill must not strand the entry
	// uncached forever because every loser gave up (S-12).
	writebackMaxAttempts = 3
	// writebackRetryBackoff is the first backoff between contention retries;
	// it doubles per attempt.
	writebackRetryBackoff = 10 * time.Millisecond
)

// writebackQueue bounds and tracks the background cache fills of the process.
type writebackQueue struct {
	slots chan struct{}

	mu       sync.Mutex
	draining bool
	wg       sync.WaitGroup
}

func newWritebackQueue(limit int) *writebackQueue {
	return &writebackQueue{slots: make(chan struct{}, limit)}
}

// submit runs fn as a bounded background cache fill on a detached context and
// reports whether it was admitted. instance, when non-nil, tracks the fill for
// callers (and tests) that await the fills they triggered. A fill submitted
// while the queue is full or draining is dropped and counted.
func (q *writebackQueue) submit(ctx context.Context, instance *sync.WaitGroup, fn func(context.Context)) bool {
	select {
	case q.slots <- struct{}{}:
	default:
		writebackDropped.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrReason, ReasonWritebackQueueFull)))

		return false
	}

	q.mu.Lock()
	if q.draining {
		q.mu.Unlock()
		<-q.slots

		writebackDropped.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrReason, ReasonWritebackDraining)))

		return false
	}
	q.wg.Add(1)
	q.mu.Unlock()

	if instance != nil {
		instance.Add(1)
	}

	writebackInFlight.Add(ctx, 1)

	go func() {
		defer q.wg.Done()
		defer func() { <-q.slots }()
		defer writebackInFlight.Add(ctx, -1)

		if instance != nil {
			defer instance.Done()
		}

		fn(context.WithoutCancel(ctx))
	}()

	return true
}

// drain refuses new admissions and waits for the in-flight fills to finish,
// bounded by ctx. Calling it again after a deadline expiry keeps waiting on
// the same in-flight set.
func (q *writebackQueue) drain(ctx context.Context) error {
	q.mu.Lock()
	q.draining = true
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("draining cache writebacks: %w", ctx.Err())
	}
}

// writebacks is the process-wide cache fill queue: every detached writeback
// (blob fill and write-through, chunk/frame/size fills) is admitted through
// it.
var writebacks = newWritebackQueue(maxConcurrentWritebacks)

// DrainWritebacks stops admitting new cache fills and waits for the in-flight
// fills to finish, bounded by ctx. The orchestrator calls it during shutdown
// so a clean stop flushes pending cache fills (REQ-C4).
func DrainWritebacks(ctx context.Context) error {
	return writebacks.drain(ctx)
}

// retryContendedLock retries acquire while it reports that another writer
// holds the cache lock, backing off between attempts; every retry is counted.
// Contention is normal cache dedup, but the losers of that race must not give
// up permanently: a writer that dies mid-fill would otherwise strand the
// entry uncached while nobody retries (S-12).
func retryContendedLock[T any](ctx context.Context, attempts int, backoff time.Duration, acquire func() (T, error)) (T, error) {
	var (
		zero T
		err  error
	)

	for attempt := range attempts {
		var value T
		value, err = acquire()
		if err == nil {
			return value, nil
		}

		if !errors.Is(err, lock.ErrLockAlreadyHeld) || attempt == attempts-1 {
			break
		}

		writebackRetries.Add(ctx, 1)

		select {
		case <-ctx.Done():
			return zero, errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}

		backoff *= 2
	}

	return zero, err
}
