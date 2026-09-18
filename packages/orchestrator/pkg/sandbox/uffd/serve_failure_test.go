//go:build linux

package uffd

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const serveFailureTimeout = 5 * time.Second

// startBrokenHandshakeTestUffd starts a backend on a private socket with no
// memfile: the FC handshake fails before the serve loop ever needs one, which
// is exactly the failure surface under test (S-27).
func startBrokenHandshakeTestUffd(t *testing.T) *Uffd {
	t.Helper()

	u := New(nil, filepath.Join(t.TempDir(), "uffd.sock"), logger.NewNopLogger())
	require.NoError(t, u.Start(t.Context()))

	return u
}

// waitForFatalExit waits for the serve loop to return and returns the error it
// reported. A failed serve loop must never look like a requested stop (nil),
// and it must not hang.
func waitForFatalExit(t *testing.T, u *Uffd) error {
	t.Helper()

	select {
	case <-u.Exit().Done():
		return u.Exit().Error()
	case <-time.After(serveFailureTimeout):
		t.Fatal("serve loop did not exit after a failed handshake")

		return nil
	}
}

// TestServeFailurePolicy: a failed serve loop is terminal and fatal (S-27).
// The failure must be observable through Exit(), every waiter must be
// released, the listener must be closed, and teardown must stay a no-op
// success — the owner (ResumeSandbox's exit watcher) stops the sandbox from
// this signal, and doStop/serveMemory teardown run unconditionally.
func TestServeFailurePolicy(t *testing.T) {
	t.Parallel()

	for name, tt := range map[string]struct {
		send          func(t *testing.T, conn net.Conn) error
		wantSubstring string
	}{
		"aborted handshake": {
			send:          func(*testing.T, net.Conn) error { return nil },
			wantSubstring: "failed to read unix msg",
		},
		"junk instead of the region mapping": {
			send: func(t *testing.T, conn net.Conn) error {
				t.Helper()

				_, err := conn.Write([]byte("not a region mapping with uffd fds"))

				return err
			},
			wantSubstring: "failed parsing memory mapping data",
		},
		"region mapping without uffd fds": {
			send: func(t *testing.T, conn net.Conn) error {
				t.Helper()

				_, err := conn.Write([]byte("[]"))

				return err
			},
			wantSubstring: "expected 1 control message",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			u := startBrokenHandshakeTestUffd(t)

			var dialer net.Dialer

			conn, err := dialer.DialContext(t.Context(), "unix", u.socketPath)
			require.NoError(t, err)

			require.NoError(t, tt.send(t, conn))
			require.NoError(t, conn.Close())

			exitErr := waitForFatalExit(t, u)
			require.Error(t, exitErr, "a failed serve loop must not look like a requested stop")
			assert.Contains(t, exitErr.Error(), tt.wantSubstring)

			// Handler waiters (resume-time prefetch/prefault callers) must be
			// released with the failure instead of hanging.
			_, prefaultErr := u.Prefault(t.Context(), 0, nil)
			require.Error(t, prefaultErr)

			// Ready closes on both outcomes so a resume gated on it cannot hang.
			select {
			case <-u.Ready():
			default:
				t.Fatal("ready channel not closed after the serve loop exited")
			}

			// A dead backend accepts no further FC connection.
			_, dialErr := dialer.DialContext(t.Context(), "unix", u.socketPath)
			require.Error(t, dialErr)

			// Teardown after a failure is a repeated no-op success.
			assert.NoError(t, u.Stop())
			assert.NoError(t, u.Stop())
		})
	}
}

// TestStopWithoutStartIsANoop: teardown paths are unconditional, so stopping a
// backend whose Start never completed must be a no-op success rather than the
// old "fdExit not set or failed" phantom cleanup error.
func TestStopWithoutStartIsANoop(t *testing.T) {
	t.Parallel()

	u := New(nil, filepath.Join(t.TempDir(), "uffd.sock"), logger.NewNopLogger())

	assert.NoError(t, u.Stop())
	assert.NoError(t, u.Stop())
}
