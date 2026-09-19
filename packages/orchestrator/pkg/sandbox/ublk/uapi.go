//go:build linux

package ublk

import "encoding/binary"

// Device paths.
const (
	controlDevicePath  = "/dev/ublk-control"
	charDevicePathFmt  = "/dev/ublkc%d"
	blockDevicePathFmt = "/dev/ublkb%d"
)

// Control command numbers (UBLK_CMD_*).
const (
	ublkCmdAddDev    = 0x04
	ublkCmdDelDev    = 0x05
	ublkCmdStartDev  = 0x06
	ublkCmdStopDev   = 0x07
	ublkCmdSetParams = 0x08

	// ublkCmdDelDevAsync deletes the device without waiting for the last
	// opener of its node to go away. ublk_cmd.h declares it as _IOR.
	ublkCmdDelDevAsync = 0x14
)

// IO command numbers (UBLK_IO_*).
const (
	ublkIOFetchReq       = 0x20
	ublkIOCommitAndFetch = 0x21
)

// Request operations, from the low byte of ublksrv_io_desc.op_flags.
const (
	ioOpRead        = 0
	ioOpWrite       = 1
	ioOpFlush       = 2
	ioOpDiscard     = 3
	ioOpWriteZeroes = 5
)

// Device feature flags (UBLK_F_*).
const (
	ublkFCmdIoctlEncode uint64 = 1 << 6
	ublkFUserCopy       uint64 = 1 << 7
	// UBLK_F_NO_AUTO_PART_SCAN (1 << 18) is deliberately not set on devices:
	// the export is a whole-disk raw device, and skipping the kernel's initial
	// partition scan is a device-behaviour change that needs the dev-VM
	// validation run first.
)

// ublk_params types (UBLK_PARAM_TYPE_*).
const (
	ublkParamTypeBasic   = 1 << 0
	ublkParamTypeDiscard = 1 << 1
)

// ABI sizes. ublk_buffer sizes are asserted by tests.
const (
	ctrlCmdSize     = 32
	ctrlDevInfoSize = 64
	ioCmdSize       = 16
	ioDescSize      = 24

	ublkMaxQueueDepth = 4096

	// ublkMaxNrQueues is UBLK_MAX_NR_QUEUES: ublk_cmd.h sizes the queue id
	// field with UBLK_QID_BITS (12), so the kernel accepts at most 4096
	// queues — the same number as the depth limit today, but a distinct limit.
	ublkMaxNrQueues = 1 << 12

	// queueIDNone marks a control command that is not for one queue. ADD_DEV
	// requires it.
	queueIDNone = 0xffff

	// maxCompletionBytes is the largest byte count a ublk completion result
	// can carry: the result field is a signed 32-bit value, and negative
	// values are read as errnos.
	maxCompletionBytes = 1<<31 - 1

	// maxDiscardSectors bounds the discard and write-zeroes requests the
	// device advertises. It is bounded by maxCompletionBytes rather than the
	// NBD transport's UINT_MAX>>9: the completion reports the served byte
	// count, so a longer request would overflow the result into an errno.
	// The block layer splits requests to the advertised bound.
	maxDiscardSectors = maxCompletionBytes / 512
)

// ublk user-copy buffer address encoding: ublk_pos() in the kernel.
const (
	ublkIOBufOffset = 0x80000000
	ublkTagOff      = 25
	ublkQIDOff      = 41
)

// Field offsets within struct ublksrv_ctrl_cmd.
const (
	ctrlCmdOffDevID   = 0
	ctrlCmdOffQueueID = 4
	ctrlCmdOffLength  = 6
	ctrlCmdOffAddr    = 8
	ctrlCmdOffData    = 16
)

// Field offsets within struct ublksrv_ctrl_dev_info.
const (
	ctrlDevInfoOffNrHWQueues    = 0
	ctrlDevInfoOffQueueDepth    = 2
	ctrlDevInfoOffIODescSize    = 6
	ctrlDevInfoOffMaxIOBufBytes = 8
	ctrlDevInfoOffDevID         = 12
	ctrlDevInfoOffFlags         = 24
)

// Field offsets within struct ublksrv_io_cmd.
const (
	ioCmdOffQID    = 0
	ioCmdOffTag    = 2
	ioCmdOffResult = 4
	ioCmdOffAddr   = 8
)

// Field offsets within struct ublksrv_io_desc.
const (
	ioDescOffOpFlags     = 0
	ioDescOffNrSectors   = 4
	ioDescOffStartSector = 8
)

// ublk_params prefix layout: len, types, basic and discard. The kernel
// zero-fills the remainder of its own struct.
const (
	paramsLen           = 60
	paramsBasicOffset   = 8
	paramsDiscardOffset = 40
)

// ioctl encoding, see _IOC in include/uapi/asm-generic/ioctl.h.
const (
	iocWrite = 1
	iocRead  = 2

	iocNRShift   = 0
	iocTypeShift = 8
	iocSizeShift = 16
	iocDirShift  = 30

	ublkIOCType = 'u'
)

func ioc(dir, nr, size uint32) uint32 {
	return dir<<iocDirShift | size<<iocSizeShift | ublkIOCType<<iocTypeShift | nr<<iocNRShift
}

// controlOp returns the cmd_op for a control command. Kernels with
// UBLK_F_CMD_IOCTL_ENCODE expect the ioctl-encoded form; older ones take the
// bare command number.
func controlOp(encoded bool, nr uint32) uint32 {
	if encoded {
		return ioc(iocRead|iocWrite, nr, ctrlCmdSize)
	}

	return nr
}

// controlOpRO is controlOp for the commands ublk_cmd.h declares as _IOR, like
// DEL_DEV_ASYNC. The driver dispatches on the command number and type only,
// but the direction is kept faithful to the header.
func controlOpRO(encoded bool, nr uint32) uint32 {
	if encoded {
		return ioc(iocRead, nr, ctrlCmdSize)
	}

	return nr
}

func dataOp(encoded bool, nr uint32) uint32 {
	if encoded {
		return ioc(iocRead|iocWrite, nr, ioCmdSize)
	}

	return nr
}

// ioPos is ublk_pos(): with UBLK_F_USER_COPY the kernel addresses request
// data through a position that encodes queue id, tag and offset into the
// tag's buffer region.
func ioPos(qID, tag uint16, off uint32) int64 {
	return int64(ublkIOBufOffset) | int64(qID)<<ublkQIDOff | int64(tag)<<ublkTagOff | int64(off)
}

// userData packs an IO tag and command number into a cqe's user_data, the
// same encoding libublksrv uses.
func userData(tag uint16, opNr uint32) uint64 {
	return uint64(tag) | uint64(opNr&0xff)<<16
}

// buildParams lays out the ublk_params prefix a device is started with.
// devSectors and maxSectors are in 512-byte units.
//
// attrs stays zero on purpose: the device is served without a write cache, so
// the block layer completes flush requests itself, exactly like the NBD
// transport, which is connected without NBD_FLAG_SEND_FLUSH.
func buildParams(devSectors uint64, maxSectors uint32, blockSize int64, discard bool) []byte {
	shift := uint8(9)
	if blockSize >= 4096 {
		shift = 12
	}

	types := uint32(ublkParamTypeBasic)
	if discard {
		types |= ublkParamTypeDiscard
	}

	length := paramsDiscardOffset
	if discard {
		length = paramsLen
	}

	b := make([]byte, length)
	le := binary.LittleEndian

	le.PutUint32(b[0:], uint32(length))
	le.PutUint32(b[4:], types)

	// struct ublk_param_basic
	le.PutUint32(b[paramsBasicOffset:], 0) // attrs: no write cache, no FUA
	b[paramsBasicOffset+4] = shift         // logical_bs_shift
	b[paramsBasicOffset+5] = shift         // physical_bs_shift
	b[paramsBasicOffset+6] = shift         // io_opt_shift
	b[paramsBasicOffset+7] = shift         // io_min_shift
	le.PutUint32(b[paramsBasicOffset+8:], maxSectors)
	le.PutUint32(b[paramsBasicOffset+12:], 0) // chunk_sectors: not zoned
	le.PutUint64(b[paramsBasicOffset+16:], devSectors)
	le.PutUint64(b[paramsBasicOffset+24:], 0) // virt_boundary_mask

	if discard {
		// struct ublk_param_discard
		le.PutUint32(b[paramsDiscardOffset:], uint32(blockSize))    // discard_alignment
		le.PutUint32(b[paramsDiscardOffset+4:], uint32(blockSize))  // discard_granularity
		le.PutUint32(b[paramsDiscardOffset+8:], maxDiscardSectors)  // max_discard_sectors
		le.PutUint32(b[paramsDiscardOffset+12:], maxDiscardSectors) // max_write_zeroes_sectors
		le.PutUint16(b[paramsDiscardOffset+16:], 1)                 // max_discard_segments
		le.PutUint16(b[paramsDiscardOffset+18:], 0)                 // reserved0
	}

	return b
}

func nextPowerOfTwo(v uint32) uint32 {
	if v < 2 {
		return 2
	}

	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16

	return v + 1
}

func roundUp(v, align int) int {
	return (v + align - 1) / align * align
}

func putU16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func putU32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func putU64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

func getU16(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }
func getU32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
func getU64(b []byte, off int) uint64 { return binary.LittleEndian.Uint64(b[off:]) }

// autoDevID asks the kernel to allocate the device number.
const autoDevID = ^uint32(0)
