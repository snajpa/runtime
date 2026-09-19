//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// queueOwner serves every tag of one hardware queue. The kernel requires the
// FETCH and COMMIT commands of a (queue, tag) pair to come from the same task
// (UBLK_F_PER_IO_DAEMON), so an owner goroutine stays locked to its OS thread
// for its whole life and never hands a tag to another task.
//
// One owner serves one request at a time; concurrency comes from the number
// of queues. The kernel still keeps up to queue_depth requests in flight: every
// tag has a fetch command pending, and the owner picks the completed ones up
// in the order they arrive.
type queueOwner struct {
	dev  *Device
	qid  uint16
	tags []uint16

	ring *ioUring
	desc []byte // kernel-written ublksrv_io_desc array of this queue
	buf  []byte // staging buffer for user-copy transfers, grown on demand

	commits []commit // scratch reused by drain, so the request loop does not allocate
}

func (o *queueOwner) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer o.dev.ownersDone.Done()

	if err := o.arm(); err != nil {
		o.dev.ownersReady.Done()
		o.dev.fail(fmt.Errorf("ublk: queue %d: arming fetches: %w", o.qid, err))

		return
	}

	o.dev.ownersReady.Done()

	for {
		if o.dev.stopping.Load() {
			return
		}

		if !o.ring.pending() {
			if err := o.ring.wait(); err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}

				o.dev.fail(fmt.Errorf("ublk: queue %d: waiting for completions: %w", o.qid, err))

				return
			}
		}

		commits, stop := o.drain()
		if stop {
			return
		}

		if len(commits) == 0 {
			continue
		}

		// Committing is what completes the request in the kernel, and
		// teardown depends on it: STOP_DEV waits for the requests already
		// dispatched to this queue, so a request that was picked up has to be
		// answered even while the device is being stopped.
		for _, c := range commits {
			o.submit(ublkIOCommitAndFetch, c.tag, c.result)
		}

		if err := o.ring.flush(); err != nil {
			o.dev.fail(fmt.Errorf("ublk: queue %d: committing requests: %w", o.qid, err))

			return
		}
	}
}

// arm submits the initial fetch of every tag. The kernel only starts a device
// once each queue has all of its tags fetched, so this runs before START_DEV.
func (o *queueOwner) arm() error {
	for _, tag := range o.tags {
		o.submit(ublkIOFetchReq, tag, 0)
	}

	return o.ring.flush()
}

// submit prepares one FETCH or COMMIT_AND_FETCH command. For a commit the
// result carries the byte count or the negative errno; with user copy the
// address field has to stay zero.
func (o *queueOwner) submit(op uint32, tag uint16, result int32) {
	sqe := o.ring.getSQE()
	sqe[sqeOffOpcode] = ioUringOpURingCmd
	putU32(sqe, sqeOffFD, uint32(o.dev.cdevFD))
	putU32(sqe, sqeOffCmdOp, dataOp(true, op))
	putU64(sqe, sqeOffUserData, userData(tag, op))
	putU16(sqe, sqeOffCmd+ioCmdOffQID, o.qid)
	putU16(sqe, sqeOffCmd+ioCmdOffTag, tag)
	putU32(sqe, sqeOffCmd+ioCmdOffResult, uint32(result))
}

type commit struct {
	tag    uint16
	result int32
}

// drain turns every completion that is already visible into the commit that
// answers it. stop reports that the owner has to exit.
func (o *queueOwner) drain() ([]commit, bool) {
	commits := o.commits[:0]

	for {
		ud, res, ok := o.ring.nextCQE()
		if !ok {
			o.commits = commits

			return commits, false
		}

		tag := uint16(ud)

		switch {
		case res == -int32(unix.ENODEV) || res == -int32(unix.ECANCELED):
			// STOP_DEV aborts the outstanding commands; that is how the queue
			// is released at teardown. It is an expected outcome, not a
			// failure to record.
			return nil, true
		case res < 0:
			if !o.dev.stopping.Load() {
				o.dev.fail(fmt.Errorf("ublk: queue %d tag %d: command failed: %w", o.qid, tag, unix.Errno(-res)))
			}

			return nil, true
		}

		commits = append(commits, commit{tag: tag, result: o.handle(tag)})
	}
}

// handle serves one request: it moves the data between the kernel's buffer
// for the tag and the backend, and returns the commit's result.
func (o *queueOwner) handle(tag uint16) int32 {
	off := int(tag) * ioDescSize
	if off+ioDescSize > len(o.desc) {
		return -int32(unix.EIO)
	}

	opFlags := getU32(o.desc, off+ioDescOffOpFlags)
	nrSectors := getU32(o.desc, off+ioDescOffNrSectors)
	startSector := getU64(o.desc, off+ioDescOffStartSector)

	op := uint8(opFlags)
	length := int64(nrSectors) * 512
	pos := int64(startSector) * 512

	if pos < 0 || length < 0 || pos+length > o.dev.size {
		return -int32(unix.EIO)
	}

	o.dev.inFlight.Add(1)
	defer o.dev.inFlight.Add(-1)

	switch op {
	case ioOpRead:
		return o.transferIn(tag, pos, length)
	case ioOpWrite:
		return o.transferOut(tag, pos, length)
	case ioOpFlush:
		// The device is exported without a write cache, so the block layer
		// completes flush requests on its own. A flush that still arrives is a
		// no-op: every request is applied before it is committed.
		return 0
	case ioOpDiscard, ioOpWriteZeroes:
		if _, err := o.dev.backend.WriteZeroesAt(pos, length); err != nil {
			return errnoResult(err)
		}

		return int32(length)
	default:
		return -int32(unix.EOPNOTSUPP)
	}
}

// transferIn serves a read: the backend into the kernel's buffer for the tag,
// which is written with pwrite because the kernel addresses it by position.
func (o *queueOwner) transferIn(tag uint16, off, length int64) int32 {
	pos := ioPos(o.qid, tag, 0)

	for done := int64(0); done < length; {
		buf := o.chunk(length - done)

		n, err := o.dev.backend.ReadAt(o.dev.ctx, buf, off+done)
		if err != nil {
			return errnoResult(err)
		}
		if n != len(buf) {
			return -int32(unix.EIO)
		}

		if err := pwriteFull(o.dev.cdevFD, buf, pos+done); err != nil {
			return errnoResult(err)
		}

		done += int64(n)
	}

	return int32(length)
}

// transferOut serves a write: the kernel's buffer for the tag, read with
// pread, into the backend.
func (o *queueOwner) transferOut(tag uint16, off, length int64) int32 {
	pos := ioPos(o.qid, tag, 0)

	for done := int64(0); done < length; {
		buf := o.chunk(length - done)

		if err := preadFull(o.dev.cdevFD, buf, pos+done); err != nil {
			return errnoResult(err)
		}

		n, err := o.dev.backend.WriteAt(buf, off+done)
		if err != nil {
			return errnoResult(err)
		}
		if n != len(buf) {
			return -int32(unix.EIO)
		}

		done += int64(len(buf))
	}

	return int32(length)
}

// chunk returns up to remaining bytes of the staging buffer, growing the
// buffer on demand. The kernel never sends a request larger than
// max_io_buf_bytes, but the loop that calls this keeps the code correct if it
// ever does.
func (o *queueOwner) chunk(remaining int64) []byte {
	size := min(remaining, int64(o.dev.opts.MaxIOBufBytes))
	if int64(len(o.buf)) < size {
		o.buf = make([]byte, nextPowerOfTwo(uint32(size)))
	}

	return o.buf[:size]
}

// preadFull reads len(buf) bytes at pos. The kernel copies whole request
// buffers per call and rejects a position past the request's data, so the
// offset it is given has to stay inside the request.
func preadFull(fd int, buf []byte, pos int64) error {
	return preadFullWith(unix.Pread, fd, buf, pos)
}

// preadFullWith is preadFull with the read syscall injected, so the retry and
// short-transfer paths are unit-testable without a ring (U-10).
func preadFullWith(readAt func(fd int, p []byte, off int64) (int, error), fd int, buf []byte, pos int64) error {
	for done := 0; done < len(buf); {
		n, err := readAt(fd, buf[done:], pos+int64(done))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}

			return err
		}
		if n == 0 {
			return unix.EIO
		}

		done += n
	}

	return nil
}

// pwriteFull writes all of buf at pos, for the same reason as preadFull.
func pwriteFull(fd int, buf []byte, pos int64) error {
	return pwriteFullWith(unix.Pwrite, fd, buf, pos)
}

// pwriteFullWith is pwriteFull with the write syscall injected, for the same
// reason as preadFullWith.
func pwriteFullWith(writeAt func(fd int, p []byte, off int64) (int, error), fd int, buf []byte, pos int64) error {
	for done := 0; done < len(buf); {
		n, err := writeAt(fd, buf[done:], pos+int64(done))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}

			return err
		}
		if n == 0 {
			return unix.EIO
		}

		done += n
	}

	return nil
}
