package storage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReclaimSandboxFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cacheDir := t.TempDir()
	matching := []string{
		filepath.Join(tmpDir, "fc-sbx-rand.sock"),
		filepath.Join(tmpDir, "uffd-sbx-rand.sock"),
		filepath.Join(tmpDir, "fc-metrics-sbx-rand.fifo"),
		filepath.Join(cacheDir, "rootfs-sbx-rand.cow"),
		filepath.Join(cacheDir, "rootfs-sbx-rand.link"),
		// The swapped-in live cache of an in-place checkpoint: named with the
		// ".cow" suffix ("…-sealN.cow") precisely so this glob keeps matching
		// — after the first fold it is the ONLY rootfs COW on disk.
		filepath.Join(cacheDir, "rootfs-sbx-rand-seal1.cow"),
	}
	for _, path := range matching {
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	}
	decoys := []string{
		filepath.Join(tmpDir, "fc.sock"),
		filepath.Join(tmpDir, "fc-sbx.sock"),
		filepath.Join(tmpDir, "uffd-sbx.sock"),
		filepath.Join(tmpDir, "fc-metrics-sbx.fifo"),
		filepath.Join(cacheDir, "rootfs-sbx.cow"),
		filepath.Join(cacheDir, "rootfs-sbx.link"),
	}
	for _, path := range decoys {
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	}

	reclaimed, failures := ReclaimSandboxFiles(tmpDir, cacheDir)
	require.Empty(t, failures)
	require.Equal(t, len(matching), reclaimed)

	for _, path := range matching {
		require.NoFileExists(t, path)
	}
	for _, path := range decoys {
		require.FileExists(t, path)
	}

	// Reclaim is idempotent: a second run finds nothing left to remove and
	// reports no failures (REQ-G3).
	reclaimed, failures = ReclaimSandboxFiles(tmpDir, cacheDir)
	require.Empty(t, failures)
	require.Zero(t, reclaimed)
}

func TestReclaimStagedDirs(t *testing.T) {
	t.Parallel()

	stageRoot := t.TempDir()

	// A stage whose owner is gone is abandoned and gets reclaimed.
	dead := filepath.Join(stageRoot, ".envd-swap-dead")
	require.NoError(t, os.Mkdir(dead, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dead, EnvdSwapStageOwnerFile), []byte("999999\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dead, "envd.new"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dead, "nested"), 0o700))

	// A stage whose owner is alive belongs to an active swap — possibly another
	// instance's on a shared host — and is never removed.
	live := filepath.Join(stageRoot, ".envd-swap-live")
	require.NoError(t, os.Mkdir(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, EnvdSwapStageOwnerFile), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600))

	// A stage without an owner record cannot be proven abandoned either; the
	// skip is logged so an operator can clear a leaked legacy directory.
	legacy := filepath.Join(stageRoot, ".envd-swap-legacy")
	require.NoError(t, os.Mkdir(legacy, 0o700))

	decoys := []string{
		filepath.Join(stageRoot, "envd-swap-300"),
		filepath.Join(stageRoot, "other"),
	}
	for _, dir := range decoys {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}

	reclaimed, failures := ReclaimStagedDirs(t.Context(), stageRoot, "/proc")
	require.Empty(t, failures)
	require.Equal(t, 1, reclaimed, "only the abandoned stage is reclaimed")
	require.NoDirExists(t, dead)
	require.DirExists(t, live, "a live owner keeps its staging directory")
	require.DirExists(t, legacy, "no owner record means no reclaim")
	for _, dir := range decoys {
		require.DirExists(t, dir)
	}

	// An empty stage root yields no patterns; the caller reports the omission.
	reclaimed, failures = ReclaimStagedDirs(t.Context(), "", "/proc")
	require.Empty(t, failures)
	require.Zero(t, reclaimed)

	// Idempotent: a second run finds nothing more to remove.
	reclaimed, failures = ReclaimStagedDirs(t.Context(), stageRoot, "/proc")
	require.Empty(t, failures)
	require.Zero(t, reclaimed)
}

// TestStageOwnerFence pins the fence itself: liveness is decided by the
// recorded pid's presence in the proc directory, and the absence of a record
// is never read as permission to delete.
func TestStageOwnerFence(t *testing.T) {
	t.Parallel()

	procDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, os.Mkdir(dir, 0o700))

	_, ok := StageOwnerAlive(dir, procDir)
	require.False(t, ok, "no owner record is not proof of abandonment")

	require.NoError(t, RecordStageOwner(dir))

	alive, ok := StageOwnerAlive(dir, procDir)
	require.True(t, ok)
	require.False(t, alive, "the recorded pid is not present in this proc dir")

	require.NoError(t, os.Mkdir(filepath.Join(procDir, strconv.Itoa(os.Getpid())), 0o700))

	alive, ok = StageOwnerAlive(dir, procDir)
	require.True(t, ok)
	require.True(t, alive)

	require.NoError(t, os.WriteFile(filepath.Join(dir, EnvdSwapStageOwnerFile), []byte("not-a-pid"), 0o600))

	_, ok = StageOwnerAlive(dir, procDir)
	require.False(t, ok, "a malformed record is not proof either")
}
