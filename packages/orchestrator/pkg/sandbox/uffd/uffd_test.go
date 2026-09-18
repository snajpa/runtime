//go:build linux

package uffd

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// testPeerEnv marks the helper process the foreign-uid test starts.
const testPeerEnv = "UFFD_TEST_PEER_SOCKET"

// TestStartCreatesPrivateSocket is the regression for the 0777 socket mode:
// after Start the socket is reachable by the orchestrator's user only.
func TestStartCreatesPrivateSocket(t *testing.T) {
	t.Parallel()

	u := New(nil, filepath.Join(t.TempDir(), "uffd-test.sock"), logger.NewNopLogger())
	require.NoError(t, u.Start(t.Context()))

	info, err := os.Stat(u.socketPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// Unblock the serve loop and let it finish, so the test leaves nothing behind.
	require.NoError(t, u.lis.Close())
	require.Error(t, waitForServeExit(t, u))
}

// TestStartRejectsExistingSocketPath: a stale or planted path fails loudly
// instead of being reused or silently unlinked.
func TestStartRejectsExistingSocketPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "uffd-test.sock")
	require.NoError(t, os.WriteFile(path, []byte("stale"), 0o600))

	err := New(nil, path, logger.NewNopLogger()).Start(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

// TestCheckSocketDir: the socket directory must be either a shared sticky
// directory (the os.TempDir() shape) or owned by the orchestrator's user
// without group/other write permission.
func TestCheckSocketDir(t *testing.T) {
	t.Parallel()

	socketDir := func(mode os.FileMode) string {
		t.Helper()

		path := t.TempDir()
		require.NoError(t, os.Chmod(path, mode))

		return path
	}

	t.Run("private directory owned by the orchestrator", func(t *testing.T) {
		t.Parallel()

		assert.NoError(t, checkSocketDir(socketDir(0o700)))
	})

	t.Run("owned directory without group or other write", func(t *testing.T) {
		t.Parallel()

		assert.NoError(t, checkSocketDir(socketDir(0o755)))
	})

	t.Run("shared sticky directory", func(t *testing.T) {
		t.Parallel()

		assert.NoError(t, checkSocketDir(socketDir(0o777|os.ModeSticky)))
	})

	t.Run("group writable directory", func(t *testing.T) {
		t.Parallel()

		assert.Error(t, checkSocketDir(socketDir(0o770)))
	})

	t.Run("world writable directory without the sticky bit", func(t *testing.T) {
		t.Parallel()

		assert.Error(t, checkSocketDir(socketDir(0o777)))
	})

	t.Run("directory owned by another user", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() != 0 {
			t.Skip("changing the owner requires root")
		}

		path := t.TempDir()
		require.NoError(t, os.Chown(path, 65534, 65534))

		assert.Error(t, checkSocketDir(path))
	})

	t.Run("missing directory", func(t *testing.T) {
		t.Parallel()

		assert.Error(t, checkSocketDir(filepath.Join(t.TempDir(), "missing")))
	})

	t.Run("regular file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "not-a-directory")
		require.NoError(t, os.WriteFile(path, nil, 0o600))

		assert.Error(t, checkSocketDir(path))
	})
}

// TestCheckPeerCreds pins the uid rule enforced on every accepted connection.
func TestCheckPeerCreds(t *testing.T) {
	t.Parallel()

	require.NoError(t, checkPeerCreds(&syscall.Ucred{Uid: uint32(os.Getuid())}))

	err := checkPeerCreds(&syscall.Ucred{Uid: uint32(os.Getuid()) + 1})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnexpectedPeer)

	require.ErrorIs(t, checkPeerCreds(nil), ErrUnexpectedPeer)
}

// TestHandleServesSameUidPeer: the ordinary path — a same-uid peer reaches the
// message read. It connects and closes without sending anything, so the read
// fails; what matters is that this is not a credential rejection.
func TestHandleServesSameUidPeer(t *testing.T) {
	t.Parallel()

	u := startTestUffd(t)

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", u.socketPath)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	serveErr := waitForServeExit(t, u)
	require.Error(t, serveErr)
	assert.NotErrorIs(t, serveErr, ErrUnexpectedPeer)
}

// TestHandleRejectsForeignUidPeer proves the defense in depth: when the socket
// is reachable by another user (the mode is deliberately relaxed here), the
// SO_PEERCRED check still rejects the connection before anything is read.
func TestHandleRejectsForeignUidPeer(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("running a helper as a different uid requires root")
	}

	u := startTestUffd(t)

	// Simulate the wrong-mode regression this check defends against, so the
	// kernel does not reject the foreign peer first.
	require.NoError(t, os.Chmod(u.socketPath, 0o666))

	allowHelperExec(t, os.Args[0])

	var helperOutput bytes.Buffer

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUffdPeerHelper$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), testPeerEnv+"="+u.socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 65534, Gid: 65534},
	}
	cmd.Stdout = &helperOutput
	cmd.Stderr = &helperOutput

	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a different-uid helper in this environment: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("different-uid helper failed: %v\n%s", err, helperOutput.String())
	}

	serveErr := waitForServeExit(t, u)
	require.Error(t, serveErr)
	assert.ErrorIs(t, serveErr, ErrUnexpectedPeer)
}

// TestUffdPeerHelper is the different-uid process the test above starts; it is
// not a test of its own and skips in a normal run.
func TestUffdPeerHelper(t *testing.T) {
	t.Parallel()

	path := os.Getenv(testPeerEnv)
	if path == "" {
		t.Skip("helper process only")
	}

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// startTestUffd starts a backend on a socket inside a shared sticky directory
// (the os.TempDir() shape) that a different-uid helper can reach.
func startTestUffd(t *testing.T) *Uffd {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o777|os.ModeSticky))
	allowTraversal(t, filepath.Dir(dir))

	u := New(nil, filepath.Join(dir, "uffd-test.sock"), logger.NewNopLogger())
	require.NoError(t, u.Start(t.Context()))

	return u
}

// waitForServeExit waits (bounded) for the serve loop to finish and returns
// its error.
func waitForServeExit(t *testing.T, u *Uffd) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	return u.exit.WaitWithContext(ctx)
}

// allowHelperExec makes this test binary executable by the different-uid
// helper. If the binary lives outside the temp dir, the helper may still be
// unable to execute it, in which case the test above skips.
func allowHelperExec(t *testing.T, binary string) {
	t.Helper()

	require.NoError(t, os.Chmod(binary, 0o755))
	allowTraversal(t, filepath.Dir(binary))
}

// allowTraversal lets a different-uid helper reach dir: a Go build or test
// temp directory is owner-only, so as root the test relaxes the path
// components between dir and os.TempDir() to traverse-only.
func allowTraversal(t *testing.T, dir string) {
	t.Helper()

	temp := os.TempDir() + string(os.PathSeparator)
	for d := dir; strings.HasPrefix(d, temp); d = filepath.Dir(d) {
		require.NoError(t, os.Chmod(d, 0o711))
	}
}
