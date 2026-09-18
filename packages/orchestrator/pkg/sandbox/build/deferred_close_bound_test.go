//go:build linux

package build

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// TestDeferredDiff_CloseBounded pins S-19: Close must not wait forever on a
// seal that never resolves.
//
//nolint:paralleltest // mutates the package-level close bound.
func TestDeferredDiff_CloseBounded(t *testing.T) {
	prev := deferredDiffCloseTimeout
	deferredDiffCloseTimeout = 50 * time.Millisecond
	t.Cleanup(func() { deferredDiffCloseTimeout = prev })

	inner := utils.NewSetOnce[Diff]()
	d := NewDeferredDiff(GetDiffStoreKey("build-id", Rootfs), 4096, inner).(*deferredDiff)

	start := time.Now()
	require.NoError(t, d.Close())
	require.Less(t, time.Since(start), time.Second, "Close must be bounded by the timeout, not hang on the promise")
}
