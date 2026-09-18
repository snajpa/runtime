//go:build linux

package nbd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	// deviceTestLockEnv overrides the cross-process lock path (the file must
	// live on storage shared by every test process of this host).
	deviceTestLockEnv = "E2B_NBD_TEST_LOCK"
	// deviceTestLockWait bounds how long a test binary waits for the devices.
	deviceTestLockWait = 20 * time.Minute
)

// deviceTestLockPath is the advisory lock every test binary that touches the
// kernel NBD devices takes. /dev/nbd* are host-global: two concurrent gate
// runs of this package attach and detach each other's devices and die with
// "failed to release device N: context deadline exceeded" plus /dev/nbd0 I/O
// errors (S-56, gate flake 2026-09-18T21:33:42Z). Validation stays concurrent
// across agents; only the device-touching test binary serializes.
func deviceTestLockPath() string {
	if path := os.Getenv(deviceTestLockEnv); path != "" {
		return path
	}

	return filepath.Join(os.TempDir(), "e2b-nbd-device-tests.lock")
}

// acquireDeviceTestLock takes the cross-process device lock, waiting up to
// wait. It reports on stderr when it has to wait so gate logs show the
// contention itself, not only the resulting flake.
func acquireDeviceTestLock(ctx context.Context, path string, wait time.Duration) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open device-test lock %s: %w", path, err)
	}

	deadline := time.Now().Add(wait)
	reported := false

	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()

			return nil, fmt.Errorf("lock device-test lock %s: %w", path, err)
		}

		if !reported {
			fmt.Fprintf(os.Stderr,
				"nbd tests: waiting for the NBD device lock %s (another test run is using /dev/nbd*)\n", path)
			reported = true
		}

		if time.Now().After(deadline) {
			_ = file.Close()

			return nil, fmt.Errorf("timed out after %s waiting for the NBD device lock %s", wait, path)
		}

		select {
		case <-ctx.Done():
			_ = file.Close()

			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func releaseDeviceTestLock(file *os.File) {
	if file == nil {
		return
	}

	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

// clearStaleDeviceAttachments disconnects devices whose recorded client process
// is gone. A killed test run leaves /dev/nbdX attached with a dead pid; the
// pool then treats the device as busy (its free check is the pid file) and
// every later run that lands on it fails with I/O errors (S-56). Call it while
// holding the device-test lock — it never touches a device a live process owns.
func clearStaleDeviceAttachments(ctx context.Context) (int, []error) {
	devices, err := ConnectedDevices()
	if err != nil {
		return 0, []error{err}
	}

	cleared := 0

	var failures []error

	for _, device := range devices {
		pid, err := deviceClientPID(device)
		if err != nil {
			failures = append(failures, err)

			continue
		}

		// Never touch a device a live process still owns: a foreign run built
		// before this lock landed must keep working.
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			continue
		}

		if err := DisconnectDevice(ctx, device); err != nil {
			failures = append(failures, fmt.Errorf("disconnect stale /dev/nbd%d (pid %d): %w", device, pid, err))

			continue
		}

		cleared++
	}

	return cleared, failures
}

// deviceClientPID reads the client pid the kernel records for an attached
// device (/sys/block/nbdX/pid). Zero means the file is empty or unparsable,
// which also counts as a stale owner.
func deviceClientPID(device DeviceSlot) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/nbd%d/pid", sysBlockDir, device))
	if err != nil {
		return 0, fmt.Errorf("read /dev/nbd%d pid: %w", device, err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, nil //nolint:nilerr // an unparsable pid is a stale owner, not a call failure
	}

	return pid, nil
}

// TestDeviceTestLock covers the helper's contract: a second holder is refused
// while the lock is held, and the lock is acquirable again after release.
func TestDeviceTestLock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "nbd.lock")

	first, err := acquireDeviceTestLock(ctx, path, time.Second)
	require.NoError(t, err)

	t.Cleanup(func() { releaseDeviceTestLock(first) })

	// A different open file description in the same process must also
	// conflict, so the test binary cannot race itself.
	_, err = acquireDeviceTestLock(ctx, path, 300*time.Millisecond)
	require.ErrorContains(t, err, "timed out")

	releaseDeviceTestLock(first)

	second, err := acquireDeviceTestLock(ctx, path, time.Second)
	require.NoError(t, err, "the lock must be acquirable after release")

	releaseDeviceTestLock(second)
}
