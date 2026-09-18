//go:build linux

package block

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/metrics"
)

func newTestBlockMetrics(t *testing.T) metrics.Metrics {
	t.Helper()

	m, err := metrics.NewMetrics(noop.NewMeterProvider())
	require.NoError(t, err)

	return m
}

func TestFetchAdmissionBoundsConcurrency(t *testing.T) {
	t.Parallel()

	m := newTestBlockMetrics(t)
	admission := newFetchAdmission(1)

	require.NoError(t, admission.acquire(t.Context(), nil, m))

	acquired := make(chan struct{})
	go func() {
		if err := admission.acquire(t.Context(), nil, m); err == nil {
			close(acquired)
			admission.release(t.Context(), m)
		}
	}()

	select {
	case <-acquired:
		t.Fatal("a second fetch must wait while the single slot is held")
	case <-time.After(50 * time.Millisecond):
	}

	admission.release(t.Context(), m)

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("the waiting fetch must proceed once the slot is released")
	}
}

func TestFetchAdmissionGrantsInArrivalOrder(t *testing.T) {
	t.Parallel()

	m := newTestBlockMetrics(t)
	admission := newFetchAdmission(1)

	require.NoError(t, admission.acquire(t.Context(), nil, m))

	const waiters = 3

	order := make(chan int, waiters)
	for id := range waiters {
		go func() {
			if err := admission.acquire(t.Context(), nil, m); err != nil {
				return
			}

			order <- id
			// Hold the slot long enough that the next waiter's position is
			// decided by arrival order, not by scheduling.
			time.Sleep(20 * time.Millisecond)
			admission.release(t.Context(), m)
		}()

		// Give each waiter time to enqueue before the next one arrives.
		time.Sleep(10 * time.Millisecond)
	}

	admission.release(t.Context(), m)

	for want := range waiters {
		select {
		case got := <-order:
			require.Equal(t, want, got, "waiters must be granted in arrival order")
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never ran", want)
		}
	}
}

func TestFetchAdmissionWaitIsBoundedByContext(t *testing.T) {
	t.Parallel()

	m := newTestBlockMetrics(t)
	admission := newFetchAdmission(1)

	require.NoError(t, admission.acquire(t.Context(), nil, m))
	defer admission.release(t.Context(), m)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, admission.acquire(ctx, nil, m), context.DeadlineExceeded)
}

func TestFetchAdmissionCancellationDoesNotLeakSlots(t *testing.T) {
	t.Parallel()

	m := newTestBlockMetrics(t)
	admission := newFetchAdmission(1)

	for range 20 {
		require.NoError(t, admission.acquire(t.Context(), nil, m))

		ctx, cancel := context.WithCancel(t.Context())

		done := make(chan error, 1)
		go func() { done <- admission.acquire(ctx, nil, m) }()

		// Let the waiter enqueue, then release and cancel together so the grant
		// and the cancellation race.
		time.Sleep(time.Millisecond)
		admission.release(t.Context(), m)
		cancel()

		if err := <-done; err == nil {
			admission.release(t.Context(), m)
		}

		// Whichever won, the gate must still be usable: no leaked slot.
		nextCtx, nextCancel := context.WithTimeout(t.Context(), time.Second)
		require.NoError(t, admission.acquire(nextCtx, nil, m))
		nextCancel()
		admission.release(t.Context(), m)
	}
}

func TestFetchAdmissionLimitResizeAdmitsWaiters(t *testing.T) {
	t.Parallel()

	m := newTestBlockMetrics(t)
	admission := newFetchAdmission(1)

	require.NoError(t, admission.acquire(t.Context(), nil, m))

	acquired := make(chan struct{})
	go func() {
		if err := admission.acquire(t.Context(), nil, m); err == nil {
			close(acquired)
			admission.release(t.Context(), m)
		}
	}()

	select {
	case <-acquired:
		t.Fatal("the waiter must not run before the limit grows")
	case <-time.After(50 * time.Millisecond):
	}

	admission.setLimit(2)

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("growing the limit must admit a waiting fetch")
	}
}
