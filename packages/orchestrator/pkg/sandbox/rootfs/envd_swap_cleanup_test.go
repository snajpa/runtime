//go:build linux

package rootfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRemoveSwapStage pins the cleanup contract: a successful removal leaves
// nothing behind, and a failing removal is reported instead of ignored — a
// leaked stage directory would keep up to maxEnvdSize of dumped host bytes.
func TestRemoveSwapStage(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()

	stage := filepath.Join(parent, "stage")
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "nested"), 0o700))

	require.NoError(t, removeSwapStage(stage))

	_, err := os.Stat(stage)
	require.ErrorIs(t, err, os.ErrNotExist, "the stage directory must be gone")

	// A path that cannot be resolved (a regular file used as a parent
	// directory) must surface an error rather than pass silently.
	regular := filepath.Join(parent, "not-a-dir")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0o600))

	require.Error(t, removeSwapStage(filepath.Join(regular, "stage")),
		"an unremovable stage path must report the failure")
}
