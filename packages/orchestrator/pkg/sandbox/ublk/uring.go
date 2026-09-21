//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// io_uring constants, from include/uapi/linux/io_uring.h.
const (
	ioUringSetupCQSIZE = 1 << 3
	ioUringSetupSQE128 = 1 << 10

	ioUringEnterGetEvents = 1 << 0

	ioUringOffSQRing = 0
	ioUringOffCQRing = 0x8000000
	ioUringOffSQEs   = 0x10000000

	ioUringFeatureSingleMmap = 1 << 0

	ioUringOpPollAdd  = 6
	ioUringOpURingCmd = 46

	// io_uring_params layout.
	ioUringParamsSize = 120

	ioUringParamsOffSQEntries = 0
	ioUringParamsOffCQEntries = 4
	ioUringParamsOffFlags     = 8
	ioUringParamsOffFeatures  = 20
	ioUringParamsOffSQTail    = 44
	ioUringParamsOffSQMak     = 48
	ioUringParamsOffSQArray   = 64
	ioUringParamsOffCQHead    = 80
	ioUringParamsOffCQTail    = 84
	ioUringParamsOffCQMask    = 88
	ioUringParamsOffCQCQEs    = 100

	cqeSize = 16

	// The control plane needs 128-byte SQEs: the driver reads the command
	// payload from the SQE itself. Data rings only carry the 16-byte
	// ublksrv_io_cmd payload, so the standard 64 bytes are enough there.
	sqe64Size  = 64
	sqe128Size = 128
)

// Field offsets within struct io_uring_sqe.
const (
	sqeOffOpcode   = 0
	sqeOffFlags    = 1
	sqeOffFD       = 4
	sqeOffCmdOp    = 8
	sqeOffAddr     = 16
	sqeOffLen      = 24
	sqeOffPollMask = 28
	sqeOffUserData = 32
	sqeOffCmd      = 48
)

// ioUring is a minimal io_uring instance: enough to issue uring_cmd
// passthrough commands and wait for their completions.
type ioUring struct {
	fd      int
	sqeSize int

	sqes   []byte
	sqRing []byte
	cqRing []byte

	sqTail  *uint32
	sqMask  uint32
	sqArray []byte

	cqHead *uint32
	cqTail *uint32
	cqMask uint32
	cqes   []byte

	singleMmap  bool
	tailLocal   uint32
	submitted   uint32
	cqHeadLocal uint32
}

func newIOUring(entries, flags uint32, sqeSize int) (*ioUring, error) {
	params := make([]byte, ioUringParamsSize)
	putU32(params, ioUringParamsOffFlags, flags|ioUringSetupCQSIZE)
	// Twice the submission ring is io_uring's default ratio. The kernel caps
	// the commands in flight at cq_entries, and a device keeps queue_depth
	// fetch commands pending, so the completion ring must not be smaller.
	putU32(params, ioUringParamsOffCQEntries, entries*2)

	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		return nil, fmt.Errorf("ublk: io_uring_setup(%d entries): %w", entries, errno)
	}

	r := &ioUring{fd: int(fd), sqeSize: sqeSize}

	sqEntries := getU32(params, ioUringParamsOffSQEntries)
	cqEntries := getU32(params, ioUringParamsOffCQEntries)
	sqArrayOff := getU32(params, ioUringParamsOffSQArray)
	cqCQEsOff := getU32(params, ioUringParamsOffCQCQEs)
	sqRingSize := int(sqArrayOff) + int(sqEntries)*4
	cqRingSize := int(cqCQEsOff) + int(cqEntries)*cqeSize

	prot := unix.PROT_READ | unix.PROT_WRITE
	mmapFlags := unix.MAP_SHARED | unix.MAP_POPULATE

	if getU32(params, ioUringParamsOffFeatures)&ioUringFeatureSingleMmap != 0 {
		size := max(sqRingSize, cqRingSize)
		ring, err := unix.Mmap(r.fd, ioUringOffSQRing, size, prot, mmapFlags)
		if err != nil {
			_ = unix.Close(r.fd)

			return nil, fmt.Errorf("ublk: mmap io_uring rings: %w", err)
		}
		r.sqRing, r.cqRing, r.singleMmap = ring, ring, true
	} else {
		sqRing, err := unix.Mmap(r.fd, ioUringOffSQRing, sqRingSize, prot, mmapFlags)
		if err != nil {
			_ = unix.Close(r.fd)

			return nil, fmt.Errorf("ublk: mmap io_uring sq ring: %w", err)
		}
		cqRing, err := unix.Mmap(r.fd, ioUringOffCQRing, cqRingSize, prot, mmapFlags)
		if err != nil {
			_ = unix.Munmap(sqRing)
			_ = unix.Close(r.fd)

			return nil, fmt.Errorf("ublk: mmap io_uring cq ring: %w", err)
		}
		r.sqRing, r.cqRing = sqRing, cqRing
	}

	sqes, err := unix.Mmap(r.fd, ioUringOffSQEs, int(sqEntries)*sqeSize, prot, mmapFlags)
	if err != nil {
		r.close()

		return nil, fmt.Errorf("ublk: mmap io_uring sqes: %w", err)
	}
	r.sqes = sqes

	r.sqTail = u32At(r.sqRing, getU32(params, ioUringParamsOffSQTail))
	r.sqMask = getU32(r.sqRing, int(getU32(params, ioUringParamsOffSQMak)))
	r.sqArray = r.sqRing[int(sqArrayOff):]
	r.cqHead = u32At(r.cqRing, getU32(params, ioUringParamsOffCQHead))
	r.cqTail = u32At(r.cqRing, getU32(params, ioUringParamsOffCQTail))
	r.cqMask = getU32(r.cqRing, int(getU32(params, ioUringParamsOffCQMask)))
	r.cqes = r.cqRing[int(cqCQEsOff):]

	return r, nil
}

func (r *ioUring) close() {
	_ = r.closeChecked()
}

func (r *ioUring) closeChecked() error {
	var errs []error
	if r.sqes != nil {
		if err := munmap(r.sqes); err != nil {
			errs = append(errs, fmt.Errorf("unmapping io_uring sqes: %w", err))
		}
		r.sqes = nil
	}
	if r.cqRing != nil && !r.singleMmap {
		if err := munmap(r.cqRing); err != nil {
			errs = append(errs, fmt.Errorf("unmapping io_uring cq ring: %w", err))
		}
	}
	r.cqRing = nil
	if r.sqRing != nil {
		if err := munmap(r.sqRing); err != nil {
			errs = append(errs, fmt.Errorf("unmapping io_uring sq ring: %w", err))
		}
		r.sqRing = nil
	}
	if r.fd > 0 {
		if err := closeFD(r.fd); err != nil {
			errs = append(errs, fmt.Errorf("closing io_uring: %w", err))
		}
		r.fd = -1
	}

	return errors.Join(errs...)
}

// getSQE returns a zeroed submission queue entry. The caller fills it and
// submits it with flush.
func (r *ioUring) getSQE() []byte {
	idx := r.tailLocal & r.sqMask
	putU32(r.sqArray, int(idx)*4, idx)

	off := int(idx) * r.sqeSize
	sqe := r.sqes[off : off+r.sqeSize]
	clear(sqe)
	r.tailLocal++

	return sqe
}

// flush publishes all prepared SQEs and submits them.
func (r *ioUring) flush() error {
	atomic.StoreUint32(r.sqTail, r.tailLocal)
	for r.submitted != r.tailLocal {
		pending := r.tailLocal - r.submitted
		n, err := r.enter(pending, 0, 0)
		if err != nil {
			return err
		}
		if n <= 0 {
			return fmt.Errorf("ublk: io_uring_enter submitted no SQEs (%d pending)", pending)
		}
		r.submitted += uint32(n)
	}

	return nil
}

// wait blocks until at least one completion is available.
func (r *ioUring) wait() error {
	for {
		_, err := r.enter(0, 1, ioUringEnterGetEvents)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		return err
	}
}

func (r *ioUring) enter(toSubmit, minComplete, flags uint32) (int, error) {
	n, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(toSubmit), uintptr(minComplete), uintptr(flags), 0, 0)
	if errno != 0 {
		return 0, errno
	}

	return int(n), nil
}

// nextCQE returns the next completion, if any.
func (r *ioUring) nextCQE() (uint64, int32, bool) {
	if r.cqHeadLocal == atomic.LoadUint32(r.cqTail) {
		return 0, 0, false
	}

	off := int(r.cqHeadLocal&r.cqMask) * cqeSize
	userData := getU64(r.cqes, off)
	res := int32(getU32(r.cqes, off+8))
	r.cqHeadLocal++
	atomic.StoreUint32(r.cqHead, r.cqHeadLocal)

	return userData, res, true
}

// pending returns the number of completions visible without entering the
// kernel.
func (r *ioUring) pending() bool {
	return r.cqHeadLocal != atomic.LoadUint32(r.cqTail)
}

func u32At(b []byte, off uint32) *uint32 {
	return (*uint32)(unsafe.Pointer(&b[off]))
}
