//go:build linux

package nbd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// acquisitionCtx returns a ctx for a test's device acquisition plus a stop
// function. The ctx is canceled when budget expires, so a starved acquisition
// fails with the device states instead of hanging to the package's 10-minute
// timeout — and a successful acquisition stops the timer, so the opened mount
// keeps a live ctx (Open retains it for its dispatchers; a deadline on the
// parent would kill them when the budget elapsed).
func acquisitionCtx(t *testing.T, budget time.Duration) (context.Context, func() bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	timer := time.AfterFunc(budget, cancel)
	t.Cleanup(cancel)

	return ctx, timer.Stop
}

// nbdDeviceStates reports each nbd device the way DevicePool.isDeviceFree
// judges it: a device is free when its pid attribute does not exist and its
// size reads 0. On a starved acquisition the report names why slots were
// skipped instead of failing with a bare timeout.
func nbdDeviceStates() string {
	maxDevices, err := getMaxDevices()
	if err != nil {
		return fmt.Sprintf("device states unavailable: %v", err)
	}

	return deviceStatesIn(sysBlockDir, maxDevices)
}

func deviceStatesIn(dir string, maxDevices uint) string {
	var b strings.Builder

	for slot := range maxDevices {
		connected, err := isDeviceConnectedIn(dir, DeviceSlot(slot))
		if err != nil {
			fmt.Fprintf(&b, "nbd%d: state unreadable (%v); ", slot, err)

			continue
		}

		sizeRaw, sizeErr := os.ReadFile(fmt.Sprintf("%s/nbd%d/size", dir, slot))

		switch {
		case connected:
			fmt.Fprintf(&b, "nbd%d: pid present; ", slot)
		case sizeErr != nil:
			fmt.Fprintf(&b, "nbd%d: size unreadable (%v); ", slot, sizeErr)
		case strings.TrimSpace(string(sizeRaw)) != "0":
			fmt.Fprintf(&b, "nbd%d: size=%s; ", slot, strings.TrimSpace(string(sizeRaw)))
		default:
			fmt.Fprintf(&b, "nbd%d: free; ", slot)
		}
	}

	return strings.TrimSuffix(b.String(), " ")
}

func TestDeviceStatesInNamesTheReasons(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nbd0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nbd0", "pid"), []byte(""), 0o644))

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nbd1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nbd1", "size"), []byte("42\n"), 0o644))

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nbd2"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nbd2", "size"), []byte("0\n"), 0o644))

	report := deviceStatesIn(dir, 3)

	require.Contains(t, report, "nbd0: pid present")
	require.Contains(t, report, "nbd1: size=42")
	require.Contains(t, report, "nbd2: free")
}

func TestAcquisitionCtxExpiresOnBudgetAndSurvivesStop(t *testing.T) {
	t.Parallel()

	starvedCtx, _ := acquisitionCtx(t, 50*time.Millisecond)

	select {
	case <-starvedCtx.Done():
		t.Fatal("the acquisition ctx must stay live before the budget expires")
	default:
	}

	time.Sleep(150 * time.Millisecond)

	select {
	case <-starvedCtx.Done():
	default:
		t.Fatal("the acquisition ctx must be canceled once the budget expires")
	}

	keptCtx, stop := acquisitionCtx(t, 50*time.Millisecond)
	require.True(t, stop(), "the budget timer must be stoppable on a successful acquisition")

	time.Sleep(150 * time.Millisecond)

	select {
	case <-keptCtx.Done():
		t.Fatal("a stopped acquisition budget must not cancel the ctx an opened mount still uses")
	default:
	}
}
