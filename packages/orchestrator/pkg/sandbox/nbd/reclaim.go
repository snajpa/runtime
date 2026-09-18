//go:build linux

package nbd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// procDir is where device-owner liveness is resolved.
const procDir = "/proc"

// ReclaimLeaked disconnects NBD devices left over from a previous orchestrator
// run. A device is only reclaimed when it has no live owner: the kernel
// publishes the owner pid in /sys/block/nbdX/pid, and a live pid may belong to
// another instance sharing the host, so those devices are skipped and logged.
// Devices whose owner cannot be attributed are treated as leaked, matching the
// previous behavior. It returns the number of devices disconnected and any
// per-device failures.
func ReclaimLeaked(ctx context.Context) (int, []error) {
	devices, err := ConnectedDevices()
	if err != nil {
		return 0, []error{err}
	}

	reclaimed := 0
	var failures []error
	for _, device := range devices {
		ownerPid, hasOwner, pidErr := deviceOwnerPidIn(sysBlockDir, device)
		if pidErr != nil {
			failures = append(failures, fmt.Errorf("failed to read the owner of nbd%d: %w", device, pidErr))

			continue
		}
		if hasOwner && deviceOwnerAliveIn(procDir, ownerPid) {
			logger.L().Warn(ctx, "not reclaiming an nbd device owned by a live process",
				zap.Int("device", int(device)), zap.Int("owner_pid", ownerPid))

			continue
		}

		if err := DisconnectDevice(ctx, device); err != nil {
			failures = append(failures, fmt.Errorf("failed to disconnect nbd%d: %w", device, err))

			continue
		}

		reclaimed++
	}

	return reclaimed, failures
}

// deviceOwnerPidIn reads the pid the kernel associates with a connected NBD
// device. hasOwner is false when the device has no pid file or the file does
// not parse: there is then no attributable live process to protect.
func deviceOwnerPidIn(blockDir string, slot DeviceSlot) (pid int, hasOwner bool, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("%s/nbd%d/pid", blockDir, slot))
	if err != nil {
		if os.IsNotExist(err) {
			// A missing pid file is the absence of an owner, not a read
			// failure: hasOwner carries that distinction to the caller.
			return 0, false, nil //nolint:nilerr // a device without a pid file is not connected, not broken
		}

		return 0, false, err
	}

	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if parseErr == nil && pid > 0 {
		return pid, true, nil
	}

	// An unparsable or non-positive pid is as unattributable as a missing file:
	// no owner to protect, and nothing a caller could act on as an error.
	return 0, false, nil
}

// deviceOwnerAliveIn reports whether pid still exists in the given proc
// directory.
func deviceOwnerAliveIn(procDir string, pid int) bool {
	_, err := os.Stat(fmt.Sprintf("%s/%d", procDir, pid))

	return err == nil
}
