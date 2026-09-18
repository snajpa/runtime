//go:build linux

package nbd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsDeviceConnectedIn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nbd0"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nbd1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nbd0", "pid"), []byte("123"), 0o600))

	connected, err := isDeviceConnectedIn(dir, 0)
	require.NoError(t, err)
	require.True(t, connected)

	connected, err = isDeviceConnectedIn(dir, 1)
	require.NoError(t, err)
	require.False(t, connected)
}

func TestDeviceOwnerPidIn(t *testing.T) {
	t.Parallel()

	blockDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(blockDir, "nbd0"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(blockDir, "nbd0", "pid"), []byte("123\n"), 0o600))

	pid, hasOwner, err := deviceOwnerPidIn(blockDir, 0)
	require.NoError(t, err)
	require.True(t, hasOwner)
	require.Equal(t, 123, pid)

	// No pid file: no attributable owner.
	pid, hasOwner, err = deviceOwnerPidIn(blockDir, 1)
	require.NoError(t, err)
	require.False(t, hasOwner)
	require.Zero(t, pid)

	// An unparseable pid file also leaves no attributable owner.
	require.NoError(t, os.Mkdir(filepath.Join(blockDir, "nbd2"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(blockDir, "nbd2", "pid"), []byte("not-a-pid"), 0o600))

	pid, hasOwner, err = deviceOwnerPidIn(blockDir, 2)
	require.NoError(t, err)
	require.False(t, hasOwner)
	require.Zero(t, pid)
}

func TestDeviceOwnerAliveIn(t *testing.T) {
	t.Parallel()

	fakeProc := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(fakeProc, "123"), 0o700))

	require.True(t, deviceOwnerAliveIn(fakeProc, 123))
	require.False(t, deviceOwnerAliveIn(fakeProc, 124))

	// The real /proc reports this process as alive.
	require.True(t, deviceOwnerAliveIn("/proc", os.Getpid()))
}
