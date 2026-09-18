//go:build linux

package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jellydator/ttlcache/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// fakeLeaseStore is the minimal Redis surface the upload lease uses: values in
// memory, call counters, and a switch that fails every operation.
type fakeLeaseStore struct {
	mu      sync.Mutex
	values  map[string]string
	sets    int
	deletes int
	err     error
}

func newFakeLeaseStore() *fakeLeaseStore {
	return &fakeLeaseStore{values: make(map[string]string)}
}

func (f *fakeLeaseStore) Set(_ context.Context, key string, value any, _ time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return redis.NewStatusResult("", f.err)
	}
	f.values[key] = value.(string)
	f.sets++

	return redis.NewStatusResult("OK", nil)
}

func (f *fakeLeaseStore) Get(_ context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return redis.NewStringResult("", f.err)
	}
	v, ok := f.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}

	return redis.NewStringResult(v, nil)
}

func (f *fakeLeaseStore) Del(_ context.Context, keys ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	var removed int64
	for _, key := range keys {
		if _, ok := f.values[key]; ok {
			delete(f.values, key)
			removed++
		}
	}
	f.deletes++

	return redis.NewIntResult(removed, nil)
}

func (f *fakeLeaseStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.values[key]

	return ok
}

func (f *fakeLeaseStore) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sets
}

// shortenLease tightens the lease TTL for a test; the refresh cadence is the
// TTL's third, so both shrink together. Sequential tests only: the timings are
// package vars.
func shortenLease(t *testing.T) {
	t.Helper()

	restore := uploadLeaseTTL
	uploadLeaseTTL = 30 * time.Millisecond
	t.Cleanup(func() { uploadLeaseTTL = restore })
}

// The lease is set while the upload runs, refreshed, and cleared when the
// upload reaches its terminal outcome (S-40).
//
//nolint:paralleltest // mutates the package-global lease TTL; must not run in parallel
func TestUploads_LeaseHeartbeatClearsOnTerminalOutcome(t *testing.T) {
	shortenLease(t)

	fake := newFakeLeaseStore()
	u, _ := newUploads(t)
	u.lease = fake

	buildID := uuid.New()
	key := uploadLeaseKey(buildID)

	fut, err := u.Start(t.Context(), buildID)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return fake.has(key) }, time.Second, 5*time.Millisecond, "lease must be set on start")
	require.Eventually(t, func() bool { return fake.setCount() >= 2 }, time.Second, 5*time.Millisecond, "lease must be refreshed while the upload runs")

	require.NoError(t, fut.SetSuccess())
	require.Eventually(t, func() bool { return !fake.has(key) }, time.Second, 5*time.Millisecond, "lease must clear on the terminal outcome")
}

// Stop ends the heartbeat and clears the lease.
//
//nolint:paralleltest // mutates the package-global lease TTL; must not run in parallel
func TestUploads_StopClearsLease(t *testing.T) {
	shortenLease(t)

	fake := newFakeLeaseStore()
	// Not started: the heartbeat only needs a futures table to register into.
	futures := ttlcache.New(ttlcache.WithTTL[uuid.UUID, *utils.ErrorOnce](futureTTL))
	u := &Uploads{lease: fake, futures: futures, stopCh: make(chan struct{})}

	buildID := uuid.New()
	key := uploadLeaseKey(buildID)

	_, err := u.Start(t.Context(), buildID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return fake.has(key) }, time.Second, 5*time.Millisecond)

	u.Stop()
	require.Eventually(t, func() bool { return !fake.has(key) }, time.Second, 5*time.Millisecond, "Stop must end the heartbeat and clear the lease")
}

// The probe maps Redis answers to liveness: present = alive, missing = known
// gone, error = inconclusive; without Redis there is no probe at all, so the
// poll keeps the plain budget (REQ-G4).
func TestUploads_LeaseProbeMapping(t *testing.T) {
	t.Parallel()

	require.Nil(t, (&Uploads{}).leaseProbe(uuid.New()), "without Redis the poll keeps the plain budget")
	(&Uploads{}).startUploadLease(t.Context(), uuid.New(), make(chan struct{})) // no Redis: must not panic

	fake := newFakeLeaseStore()
	u := &Uploads{lease: fake}
	buildID := uuid.New()
	probe := u.leaseProbe(buildID)
	require.NotNil(t, probe)

	alive, known := probe(t.Context())
	require.False(t, alive)
	require.True(t, known, "a missing lease is a known answer")

	require.NoError(t, fake.Set(t.Context(), uploadLeaseKey(buildID), "1", time.Second).Err())
	alive, known = probe(t.Context())
	require.True(t, alive)
	require.True(t, known)

	fake.mu.Lock()
	fake.err = errors.New("redis down")
	fake.mu.Unlock()

	alive, known = probe(t.Context())
	require.False(t, alive)
	require.False(t, known, "an error must be inconclusive so Redis trouble never fails a wait early")
}
