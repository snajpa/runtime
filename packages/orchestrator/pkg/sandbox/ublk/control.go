//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// controlRingEntries is small: control commands are issued one at a time.
const controlRingEntries = 8

// controlUserData tags control completions. Only one command is in flight at
// a time, so any value works; this one spells "ublk".
const controlUserData = 0x75626c6b

// ctrlCmd is struct ublksrv_ctrl_cmd, the payload of a control command.
type ctrlCmd struct {
	devID   uint32
	queueID uint16
	length  uint16
	addr    uint64
	data0   uint64
}

func (c ctrlCmd) marshal(b []byte) {
	putU32(b, ctrlCmdOffDevID, c.devID)
	putU16(b, ctrlCmdOffQueueID, c.queueID)
	putU16(b, ctrlCmdOffLength, c.length)
	putU64(b, ctrlCmdOffAddr, c.addr)
	putU64(b, ctrlCmdOffData, c.data0)
}

// sliceAddr is the address of the first byte of b, for the commands that hand
// the kernel a pointer.
func sliceAddr(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}

	return uint64(uintptr(unsafe.Pointer(&b[0])))
}

// controlPlane issues uring_cmd commands on /dev/ublk-control. The driver
// requires an SQE128 ring there, and reads the command payload from the SQE
// itself, so the SQEs are allocated at 128 bytes.
type controlPlane struct {
	fd   int
	ring *ioUring
}

func openControlPlane() (*controlPlane, error) {
	fd, err := unix.Open(controlDevicePath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ublk: opening %s: %w", controlDevicePath, err)
	}

	ring, err := newIOUring(controlRingEntries, ioUringSetupSQE128, sqe128Size)
	if err != nil {
		_ = unix.Close(fd)

		return nil, err
	}

	return &controlPlane{fd: fd, ring: ring}, nil
}

func (c *controlPlane) close() {
	if c.ring != nil {
		c.ring.close()
		c.ring = nil
	}

	if c.fd >= 0 {
		_ = unix.Close(c.fd)
		c.fd = -1
	}
}

// command submits one control command and waits for its completion. Control
// commands are synchronous and unbounded: the kernel completes one when the
// operation it drives finishes, so a device wedged inside the driver blocks its
// command, and because the ring serves one caller at a time the whole manager
// with it. The paths that could wait on a device node that is still open use
// the asynchronous delete instead (see docs/ublk-transport.md). op is the
// encoded cmd_op.
func (c *controlPlane) command(op uint32, cmd ctrlCmd) error {
	sqe := c.ring.getSQE()
	sqe[sqeOffOpcode] = ioUringOpURingCmd
	putU32(sqe, sqeOffFD, uint32(c.fd))
	putU32(sqe, sqeOffCmdOp, op)
	putU64(sqe, sqeOffUserData, controlUserData)
	cmd.marshal(sqe[sqeOffCmd:])

	if err := c.ring.flush(); err != nil {
		return fmt.Errorf("ublk: submitting control command %#x: %w", op, err)
	}

	for {
		if !c.ring.pending() {
			if err := c.ring.wait(); err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}

				return fmt.Errorf("ublk: waiting for control command %#x: %w", op, err)
			}

			continue
		}

		if _, res, ok := c.ring.nextCQE(); ok {
			if res < 0 {
				return fmt.Errorf("ublk: control command %#x: %w", op, unix.Errno(-res))
			}

			return nil
		}
	}
}
