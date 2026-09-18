//go:build linux

package block

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestMemfdCacheReadsBoundedByWaitTimeout pins S-19: the io.ReaderAt-shaped
// reads cannot carry a caller context, so their wait for the background copy
// must be bounded — an unresolved copy fails the read instead of hanging.
//
//nolint:paralleltest // mutates the package-level wait bound.
func TestMemfdCacheReadsBoundedByWaitTimeout(t *testing.T) {
	prev := memfdWaitTimeout
	memfdWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { memfdWaitTimeout = prev })

	m := &MemfdCache{done: utils.NewSetOnce[struct{}]()}

	_, err := m.ReadAt(make([]byte, 32), 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = m.Slice(0, 32)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestDedupedMemfdCacheReadsBoundedByWaitTimeout is the deduped twin: its
// drain wait must be bounded too.
//
//nolint:paralleltest // mutates the package-level wait bound.
func TestDedupedMemfdCacheReadsBoundedByWaitTimeout(t *testing.T) {
	prev := memfdWaitTimeout
	memfdWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { memfdWaitTimeout = prev })

	d := &DedupedMemfdCache{done: utils.NewSetOnce[*Cache]()}

	_, err := d.ReadAt(make([]byte, 32), 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = d.Slice(0, 32)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestDedupedMemfdCacheCloseSurfacesDrainError pins S-19: a failed drain must
// be reported by Close instead of being discarded.
func TestDedupedMemfdCacheCloseSurfacesDrainError(t *testing.T) {
	t.Parallel()

	drainErr := errors.New("drain failed")

	d := &DedupedMemfdCache{
		outPath: filepath.Join(t.TempDir(), "missing"),
		cancel:  func() {},
		done:    utils.NewSetOnce[*Cache](),
	}
	require.NoError(t, d.done.SetError(drainErr))

	err := d.Close()
	require.ErrorIs(t, err, drainErr)
}
