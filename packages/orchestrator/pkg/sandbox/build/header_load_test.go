//go:build linux

package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// pollStorage is a StorageProvider whose header object is absent, visible or
// failing depending on the test's needs. The poll only calls OpenBlob.
type pollStorage struct {
	storage.StorageProvider // embedded nil: the unused operations are never called

	mu      sync.Mutex
	present bool
	blob    storage.Blob
	err     error
}

func (p *pollStorage) OpenBlob(_ context.Context, _ string) (storage.Blob, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case p.err != nil:
		return nil, p.err
	case !p.present:
		return nil, storage.ErrObjectNotExist
	default:
		return p.blob, nil
	}
}

func (p *pollStorage) publish(blob storage.Blob) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.present = true
	p.blob = blob
}

func (p *pollStorage) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.err = err
}

// serializedBlob serves raw bytes through a mock blob, the shape LoadHeader
// reads.
func serializedBlob(t *testing.T, raw []byte) storage.Blob {
	t.Helper()

	blob := storage.NewMockBlob(t)
	blob.EXPECT().
		WriteTo(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, w io.Writer) (int64, error) {
			return io.Copy(w, bytes.NewReader(raw))
		}).Maybe()

	return blob
}

// A visible header ends the wait on the first iteration.
func TestPollRemoteStorageForHeader_ReturnsHeaderWhenVisible(t *testing.T) {
	t.Parallel()

	buildID := uuid.New()
	raw, err := header.SerializeHeader(buildHeader(t, buildID, 4096, buildID))
	require.NoError(t, err)

	provider := &pollStorage{}
	provider.publish(serializedBlob(t, raw))

	got, err := PollRemoteStorageForHeader(t.Context(), provider, buildID, Memfile, nil, time.Second, nil)
	require.NoError(t, err)
	require.Equal(t, buildID, got.Metadata.BuildId)
}

// Without a probe (or with a probe that never sees the lease alive) the flat
// budget stays the only bound.
func TestPollRemoteStorageForHeader_BudgetExpiryWithoutProbe(t *testing.T) {
	t.Parallel()

	_, err := PollRemoteStorageForHeader(t.Context(), &pollStorage{}, uuid.New(), Memfile, nil, 50*time.Millisecond, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUploaderGone)
	require.ErrorContains(t, err, "not visible after")
}

// The upload-done hint is terminal: an upload failure ends the wait at once.
func TestPollRemoteStorageForHeader_HintFailureIsTerminal(t *testing.T) {
	t.Parallel()

	hint := make(chan error, 1)
	hint <- errors.New("upload failed: disk full")

	_, err := PollRemoteStorageForHeader(t.Context(), &pollStorage{}, uuid.New(), Memfile, hint, time.Second, nil)
	require.ErrorContains(t, err, "upload signaled failure")
}

// Non-NotFound load errors are tolerated only up to the transient-error cap.
func TestPollRemoteStorageForHeader_TransientErrorCap(t *testing.T) {
	t.Parallel()

	provider := &pollStorage{}
	provider.fail(errors.New("storage hiccup"))

	_, err := PollRemoteStorageForHeader(t.Context(), provider, uuid.New(), Memfile, nil, time.Second, nil)
	require.ErrorContains(t, err, "after 3 attempts")
}

// The uploader lease arms the poll and then fails it well inside the budget:
// observed alive first, then gone for the whole grace.
//
//nolint:paralleltest // mutates the package-global probe/grace vars; must not run in parallel
func TestPollRemoteStorageForHeader_FailsFastWhenUploaderLeaseLost(t *testing.T) {
	restoreInterval := loadV4ProbeInterval
	loadV4ProbeInterval = 10 * time.Millisecond
	t.Cleanup(func() { loadV4ProbeInterval = restoreInterval })
	restoreGrace := loadV4LeaseGoneGrace
	loadV4LeaseGoneGrace = 60 * time.Millisecond
	t.Cleanup(func() { loadV4LeaseGoneGrace = restoreGrace })

	probes := 0
	probe := func(context.Context) (bool, bool) {
		probes++
		if probes == 1 {
			return true, true // the uploader is alive…
		}

		return false, true // …until its lease disappears.
	}

	start := time.Now()
	_, err := PollRemoteStorageForHeader(t.Context(), &pollStorage{}, uuid.New(), Memfile, nil, 10*time.Second, probe)
	require.ErrorIs(t, err, ErrUploaderGone)
	require.ErrorContains(t, err, "disappeared")
	require.Less(t, time.Since(start), 5*time.Second, "must fail inside the grace, not the budget")
}

// expectBudgetBoundPoll runs a probe-driven poll with a tiny budget and pins
// the no-early-failure outcome, returning how often the probe ran.
func expectBudgetBoundPoll(t *testing.T, answer func(context.Context) (bool, bool)) int {
	t.Helper()

	restoreInterval := loadV4ProbeInterval
	loadV4ProbeInterval = 5 * time.Millisecond
	t.Cleanup(func() { loadV4ProbeInterval = restoreInterval })
	restoreGrace := loadV4LeaseGoneGrace
	loadV4LeaseGoneGrace = 10 * time.Millisecond
	t.Cleanup(func() { loadV4LeaseGoneGrace = restoreGrace })

	probes := 0
	probe := func(ctx context.Context) (bool, bool) {
		probes++

		return answer(ctx)
	}

	_, err := PollRemoteStorageForHeader(t.Context(), &pollStorage{}, uuid.New(), Memfile, nil, 200*time.Millisecond, probe)
	require.ErrorContains(t, err, "not visible after")
	require.NotErrorIs(t, err, ErrUploaderGone)

	return probes
}

// A never-alive lease reads as absent, but absence alone must not arm the
// early failure: the uploader may live in a fleet without the lease mechanism.
//
//nolint:paralleltest // mutates the package-global probe/grace vars; must not run in parallel
func TestPollRemoteStorageForHeader_AbsentProbeKeepsBudget(t *testing.T) {
	probes := expectBudgetBoundPoll(t, func(context.Context) (bool, bool) { return false, true })
	require.Positive(t, probes, "the probe ran; only an observed live-then-gone lease may fail the wait")
}

// Inconclusive probe answers (Redis trouble) must never fail a wait early.
//
//nolint:paralleltest // mutates the package-global probe/grace vars; must not run in parallel
func TestPollRemoteStorageForHeader_InconclusiveProbeKeepsBudget(t *testing.T) {
	probes := expectBudgetBoundPoll(t, func(context.Context) (bool, bool) { return false, false })
	require.Positive(t, probes, "the probe ran; an inconclusive answer must never fail the wait")
}
