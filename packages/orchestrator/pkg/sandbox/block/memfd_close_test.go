//go:build linux

package block

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestMemfdCacheCloseBounded pins S-33: a copy that outlives the bound must
// not hang Close; the cache's close is handed to the copy goroutine instead.
//
//nolint:paralleltest // mutates the package-level wait bound.
func TestMemfdCacheCloseBounded(t *testing.T) {
	prev := memfdWaitTimeout
	memfdWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { memfdWaitTimeout = prev })

	m := &MemfdCache{cancel: func() {}, done: utils.NewSetOnce[struct{}]()}

	err := m.Close()
	require.Error(t, err, "Close must be bounded, not hang on the copy")
	require.Contains(t, err.Error(), "did not finish")
	require.True(t, m.closeAfterCopy.Load(), "the timed-out Close hands the cache's close to the copy")
}

// TestDedupedMemfdCacheCloseBounded pins S-33 on the deduped cache: a drain
// that never resolves must not hang Close either.
//
//nolint:paralleltest // mutates the package-level wait bound.
func TestDedupedMemfdCacheCloseBounded(t *testing.T) {
	prev := memfdWaitTimeout
	memfdWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { memfdWaitTimeout = prev })

	d := &DedupedMemfdCache{cancel: func() {}, done: utils.NewSetOnce[*Cache]()}

	err := d.Close()
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not finish")
}
