//go:build linux

package ublk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Options configure a ublk device.
type Options struct {
	// Queues is the number of hardware queues. Every queue is served by one
	// dedicated daemon task, so the device serves Queues requests at a time.
	Queues int

	// QueueDepth is the number of tags per queue: how many requests the
	// kernel may have in flight on one queue.
	QueueDepth int

	// MaxIOBufBytes caps a single request. max_sectors is derived from it, so
	// the kernel never sends a larger one.
	MaxIOBufBytes int

	// BlockSize is the logical block size advertised to the kernel. It has to
	// match the backend's block size.
	BlockSize int64

	// Discard advertises discard and write-zeroes support. It is not defaulted
	// from the zero value: use DefaultOptions to get the transport defaults.
	Discard bool
}

const (
	defaultQueues        = 1
	defaultQueueDepth    = 64
	defaultMaxIOBufBytes = 4 << 20
	defaultBlockSize     = 4096

	// maxIOBufBytes is the size of the per-tag buffer region the driver
	// reserves.
	maxIOBufBytes = 32 << 20

	queueReadyTimeout = 30 * time.Second
	deviceNodeTimeout = 5 * time.Second
	ownerExitGrace    = 2 * time.Second
	waitPollInterval  = time.Millisecond
)

// DefaultOptions are the settings a device is created with unless a feature
// flag overrides them.
func DefaultOptions() Options {
	return Options{
		Queues:        defaultQueues,
		QueueDepth:    defaultQueueDepth,
		MaxIOBufBytes: defaultMaxIOBufBytes,
		BlockSize:     defaultBlockSize,
		Discard:       true,
	}
}

func (o Options) normalized() (Options, error) {
	if o.Queues == 0 {
		o.Queues = defaultQueues
	}
	if o.QueueDepth == 0 {
		o.QueueDepth = defaultQueueDepth
	}
	if o.MaxIOBufBytes == 0 {
		o.MaxIOBufBytes = defaultMaxIOBufBytes
	}
	if o.BlockSize == 0 {
		o.BlockSize = defaultBlockSize
	}

	switch {
	case o.Queues < 1 || o.Queues > ublkMaxNrQueues:
		return o, fmt.Errorf("ublk: invalid queue count %d", o.Queues)
	case o.QueueDepth < 1 || o.QueueDepth > ublkMaxQueueDepth:
		return o, fmt.Errorf("ublk: invalid queue depth %d", o.QueueDepth)
	case o.MaxIOBufBytes < int(o.BlockSize) || o.MaxIOBufBytes > maxIOBufBytes:
		return o, fmt.Errorf("ublk: invalid max I/O buffer size %d", o.MaxIOBufBytes)
	case o.BlockSize != 512 && o.BlockSize != 4096:
		return o, fmt.Errorf("ublk: unsupported block size %d", o.BlockSize)
	}

	return o, nil
}

// Manager owns the control plane and creates devices. One Manager serves a
// process and is safe for concurrent use.
type Manager struct {
	ctrl *controlPlane

	mu     sync.Mutex
	closed bool
}

// NewManager opens /dev/ublk-control. It fails when the ublk driver is not
// loaded, which lets a caller fall back to another transport.
func NewManager() (*Manager, error) {
	ctrl, err := openControlPlane()
	if err != nil {
		return nil, err
	}

	return &Manager{ctrl: ctrl}, nil
}

// Close releases the control plane. Devices have to be closed first.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true
	m.ctrl.close()

	return nil
}

// command issues one of the _IOWR control commands by its number.
func (m *Manager) command(nr uint32, cmd ctrlCmd) error {
	return m.commandOp(controlOp(true, nr), cmd)
}

// commandOp issues a control command with an explicit encoded cmd_op, for the
// commands whose header declares a direction other than _IOWR.
func (m *Manager) commandOp(op uint32, cmd ctrlCmd) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return errors.New("ublk: manager is closed")
	}

	return m.ctrl.command(op, cmd)
}

// Device is a ublk block device whose I/O this process serves.
type Device struct {
	mgr     *Manager
	backend Backend
	opts    Options
	size    int64

	id   uint32
	path string

	//nolint:containedctx // the device owns the data path's lifetime: its
	// context deliberately outlives the call (and the request) that created it.
	ctx    context.Context
	cancel context.CancelFunc

	cdevFD    int
	blockFD   int
	queueBufs [][]byte

	owners      []*queueOwner
	ownersReady sync.WaitGroup
	ownersDone  sync.WaitGroup

	added    atomic.Bool
	started  atomic.Bool
	stopping atomic.Bool
	inFlight atomic.Int64
	invalid  atomic.Pointer[error]

	mu     sync.Mutex
	closed bool
}

// Open creates the device, starts it and returns it with its node ready, so
// the path can go straight to Firecracker. Close deletes the device.
func (m *Manager) Open(ctx context.Context, backend Backend, opts Options) (*Device, error) {
	opts, err := opts.normalized()
	if err != nil {
		return nil, err
	}

	size, err := backend.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("ublk: reading backend size: %w", err)
	}
	if size <= 0 || size%opts.BlockSize != 0 {
		return nil, fmt.Errorf("ublk: backend size %d is not a positive multiple of %d", size, opts.BlockSize)
	}

	d := &Device{mgr: m, backend: backend, opts: opts, size: size, cdevFD: -1, blockFD: -1}
	// The data path outlives the request that created the sandbox, so the
	// device context only ends with the device itself.
	d.ctx, d.cancel = context.WithCancel(context.WithoutCancel(ctx))

	if err := d.create(); err != nil {
		_ = d.teardown(ctx, false)

		return nil, err
	}

	if err := d.start(); err != nil {
		_ = d.teardown(ctx, false)

		return nil, err
	}

	return d, nil
}

// Path is the block device node the device is exported as.
func (d *Device) Path() string { return d.path }

// ID is the device number the kernel assigned.
func (d *Device) ID() uint32 { return d.id }

// Options returns the device's effective options.
func (d *Device) Options() Options { return d.opts }

// Failure reports why the device stopped serving I/O, if it did. A device
// that failed has to be closed and replaced.
func (d *Device) Failure() error {
	if p := d.invalid.Load(); p != nil {
		return *p
	}

	return nil
}

// create adds the device and brings its data path up: parameters, the
// character device, the command buffers and the queue tasks.
func (d *Device) create() error {
	info := make([]byte, ctrlDevInfoSize)
	putU16(info, ctrlDevInfoOffNrHWQueues, uint16(d.opts.Queues))
	putU16(info, ctrlDevInfoOffQueueDepth, uint16(d.opts.QueueDepth))
	putU16(info, ctrlDevInfoOffIODescSize, ioDescSize)
	putU32(info, ctrlDevInfoOffMaxIOBufBytes, uint32(d.opts.MaxIOBufBytes))
	putU32(info, ctrlDevInfoOffDevID, autoDevID)
	putU64(info, ctrlDevInfoOffFlags, ublkFUserCopy)

	cmd := ctrlCmd{
		devID:   autoDevID,
		queueID: queueIDNone,
		length:  ctrlDevInfoSize,
		addr:    sliceAddr(info),
	}

	if err := d.mgr.command(ublkCmdAddDev, cmd); err != nil {
		return fmt.Errorf("ublk: adding device: %w", err)
	}
	d.added.Store(true)

	// The kernel writes the allocated device id and the flags it negotiated
	// back into the buffer it was given.
	d.id = getU32(info, ctrlDevInfoOffDevID)
	flags := getU64(info, ctrlDevInfoOffFlags)
	runtime.KeepAlive(info)

	if flags&ublkFUserCopy == 0 || flags&ublkFCmdIoctlEncode == 0 {
		return fmt.Errorf("ublk: device %d: kernel did not enable user copy and ioctl-encoded commands (flags %#x)", d.id, flags)
	}

	if err := d.setParams(); err != nil {
		return err
	}

	cdevPath := fmt.Sprintf(charDevicePathFmt, d.id)

	cdevFD, err := unix.Open(cdevPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("ublk: opening %s: %w", cdevPath, err)
	}
	d.cdevFD = cdevFD

	if err := d.mapQueueBuffers(); err != nil {
		return err
	}

	return d.startOwners()
}

func (d *Device) setParams() error {
	maxSectors := uint32(d.opts.MaxIOBufBytes >> 9)
	params := buildParams(uint64(d.size/512), maxSectors, d.opts.BlockSize, d.opts.Discard)

	cmd := ctrlCmd{
		devID:   d.id,
		queueID: queueIDNone,
		length:  uint16(len(params)),
		addr:    sliceAddr(params),
	}

	if err := d.mgr.command(ublkCmdSetParams, cmd); err != nil {
		return fmt.Errorf("ublk: setting parameters of device %d: %w", d.id, err)
	}
	runtime.KeepAlive(params)

	return nil
}

// mapQueueBuffers maps the per-queue command buffers. The kernel writes the
// descriptors there and rejects writable mappings. The stride between queues
// is sized for the maximum queue depth, not for this device's.
func (d *Device) mapQueueBuffers() error {
	pageSize := unix.Getpagesize()
	stride := roundUp(ublkMaxQueueDepth*ioDescSize, pageSize)
	size := roundUp(d.opts.QueueDepth*ioDescSize, pageSize)

	d.queueBufs = make([][]byte, d.opts.Queues)

	for q := range d.queueBufs {
		buf, err := unix.Mmap(d.cdevFD, int64(q*stride), size, unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("ublk: mapping the command buffer of queue %d: %w", q, err)
		}

		d.queueBufs[q] = buf
	}

	return nil
}

func (d *Device) startOwners() error {
	owners := make([]*queueOwner, 0, d.opts.Queues)

	for q := range d.opts.Queues {
		ring, err := newIOUring(nextPowerOfTwo(uint32(d.opts.QueueDepth)), 0, sqe64Size)
		if err != nil {
			for _, owner := range owners {
				owner.ring.close()
			}

			return fmt.Errorf("ublk: queue %d: %w", q, err)
		}

		owner := &queueOwner{
			dev:  d,
			qid:  uint16(q),
			ring: ring,
			desc: d.queueBufs[q],
			tags: make([]uint16, d.opts.QueueDepth),
		}
		for tag := range owner.tags {
			owner.tags[tag] = uint16(tag)
		}

		owners = append(owners, owner)
	}

	d.owners = owners

	for _, owner := range owners {
		d.ownersReady.Add(1)
		d.ownersDone.Add(1)

		go owner.run()
	}

	return nil
}

func (d *Device) start() error {
	// The kernel starts a device only once every queue has all of its tags
	// fetched, so the queue tasks have to arm before START_DEV is sent.
	if err := d.waitOwnersReady(); err != nil {
		return err
	}

	// The kernel validates the daemon pid against the process that opened the
	// character device.
	cmd := ctrlCmd{devID: d.id, queueID: queueIDNone, data0: uint64(os.Getpid())}
	if err := d.mgr.command(ublkCmdStartDev, cmd); err != nil {
		return fmt.Errorf("ublk: starting device %d: %w", d.id, err)
	}
	d.started.Store(true)

	d.path = fmt.Sprintf(blockDevicePathFmt, d.id)

	fd, err := openDeviceNode(d.path)
	if err != nil {
		return err
	}
	d.blockFD = fd

	return nil
}

// openDeviceNode waits for the kernel to publish the block device node.
func openDeviceNode(path string) (int, error) {
	deadline := time.Now().Add(deviceNodeTimeout)

	for {
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.ENOENT) {
			return -1, fmt.Errorf("ublk: opening %s: %w", path, err)
		}

		if time.Now().After(deadline) {
			return -1, fmt.Errorf("ublk: %s did not appear", path)
		}

		time.Sleep(waitPollInterval)
	}
}

// waitNodeGone waits for the kernel to take the device node away.
func waitNodeGone(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("ublk: %s is still present", path)
		}

		time.Sleep(waitPollInterval)
	}
}

func (d *Device) waitOwnersReady() error {
	ready := make(chan struct{})

	go func() {
		d.ownersReady.Wait()
		close(ready)
	}()

	select {
	case <-ready:
	case <-time.After(queueReadyTimeout):
		return fmt.Errorf("ublk: queues of device %d did not arm in time", d.id)
	}

	return d.Failure()
}

// Sync makes every write the kernel acknowledged visible in the backend and
// surfaces writeback failures, then waits for the requests being served. It is
// the counterpart of the NBD transport's flush: the device is exported without
// a write cache, so the block layer completes empty flush requests on its own
// and this sync is what reports a write the kernel could not deliver.
func (d *Device) Sync(ctx context.Context) error {
	if err := d.Failure(); err != nil {
		return err
	}

	if !d.started.Load() || d.blockFD < 0 {
		return nil
	}

	// The descriptor opened at start is the one whose mapping records
	// writeback errors, so it is the one to sync, like the NBD transport's.
	syncErr := unix.Fsync(d.blockFD)

	// Invalidate even when the sync failed: the device is about to be exported
	// or reused, and stale pages must not outlive the error.
	invalidateErr := unix.IoctlSetInt(d.blockFD, unix.BLKFLSBUF, 0)

	if err := d.waitInFlight(ctx); err != nil {
		return err
	}

	if err := d.Failure(); err != nil {
		return err
	}

	var errs []error
	if syncErr != nil {
		errs = append(errs, fmt.Errorf("ublk: syncing %s: %w", d.path, syncErr))
	}
	if invalidateErr != nil {
		errs = append(errs, fmt.Errorf("ublk: invalidating %s: %w", d.path, invalidateErr))
	}

	return errors.Join(errs...)
}

// Close syncs, stops and deletes the device.
func (d *Device) Close(ctx context.Context) error {
	return d.teardown(ctx, true)
}

// teardown takes the device down in the order the kernel expects: stop the
// data path, release the character device, delete the device. It is safe on
// partially created devices and can be called more than once.
func (d *Device) teardown(ctx context.Context, barrier bool) error {
	if !d.claim() {
		return nil
	}

	var errs []error

	// The barrier runs while the data path still serves, so writes the kernel
	// still holds in its page cache reach the backend through it.
	if barrier && d.started.Load() {
		if err := d.Sync(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	// Release the device node before stopping: the kernel's teardown of the
	// node then runs against a data path that is still alive.
	if err := d.closeBlockDevice(); err != nil {
		errs = append(errs, err)
	}

	// Stopping the device aborts every outstanding fetch and commit command,
	// which is what releases the queue tasks. A device that was never started
	// (or already stopped) reports -ENODEV and still has to be deleted.
	if d.added.Load() {
		if err := d.mgr.command(ublkCmdStopDev, ctrlCmd{devID: d.id, queueID: queueIDNone}); err != nil && !errors.Is(err, unix.ENODEV) {
			errs = append(errs, fmt.Errorf("ublk: stopping device %d: %w", d.id, err))
		}
	}

	// The queue tasks are told to exit only after the stop: until then the
	// kernel is waiting for the requests it already dispatched to them, and a
	// task that walked away from one would keep STOP_DEV from ever finishing.
	d.stopping.Store(true)

	if !d.waitOwners(ownerExitGrace) {
		// A queue task is still inside a backend call: abort it and give the
		// task a second grace period to unwind.
		d.cancel()

		if !d.waitOwners(ownerExitGrace) {
			errs = append(errs, fmt.Errorf("ublk: queue tasks of device %d did not exit", d.id))

			// The device must not be stranded, and deleting it cancels what
			// the tasks are stuck on. Their descriptors stay open.
			if err := d.delete(); err != nil {
				errs = append(errs, err)
			}

			return errors.Join(errs...)
		}
	}

	for _, owner := range d.owners {
		owner.ring.close()
	}

	for _, buf := range d.queueBufs {
		_ = unix.Munmap(buf)
	}
	d.queueBufs = nil

	if err := d.closeCdev(); err != nil {
		errs = append(errs, err)
	}

	if err := d.delete(); err != nil {
		errs = append(errs, err)
	} else if d.path != "" {
		// The node goes away with the device; the next device should not have
		// to race the old node.
		if err := waitNodeGone(d.path, deviceNodeTimeout); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (d *Device) claim() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return false
	}
	d.closed = true

	return true
}

// delete removes the device from the kernel. The asynchronous command is used
// first: the synchronous one waits for the last opener of the device node to
// go away, which would make teardown unbounded while the node is still open.
func (d *Device) delete() error {
	if !d.added.Load() {
		return nil
	}

	cmd := ctrlCmd{devID: d.id, queueID: queueIDNone}

	err := d.mgr.commandOp(controlOpRO(true, ublkCmdDelDevAsync), cmd)
	if err == nil {
		return nil
	}

	if !errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("ublk: deleting device %d: %w", d.id, err)
	}

	// Kernels without DEL_DEV_ASYNC: the synchronous command only returns once
	// every opener has released the node.
	if err := d.mgr.command(ublkCmdDelDev, cmd); err != nil {
		return fmt.Errorf("ublk: deleting device %d: %w", d.id, err)
	}

	return nil
}

func (d *Device) closeBlockDevice() error {
	if d.blockFD < 0 {
		return nil
	}

	err := unix.Close(d.blockFD)
	d.blockFD = -1

	if err != nil {
		return fmt.Errorf("ublk: closing the device node: %w", err)
	}

	return nil
}

func (d *Device) closeCdev() error {
	if d.cdevFD < 0 {
		return nil
	}

	err := unix.Close(d.cdevFD)
	d.cdevFD = -1

	if err != nil {
		return fmt.Errorf("ublk: closing the character device: %w", err)
	}

	return nil
}

func (d *Device) waitOwners(timeout time.Duration) bool {
	done := make(chan struct{})

	go func() {
		d.ownersDone.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitInFlight waits until no request is being served, which is what makes the
// sync a barrier for requests already inside the data path.
func (d *Device) waitInFlight(ctx context.Context) error {
	for {
		if d.inFlight.Load() == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// fail records why the device stopped serving I/O and releases the resources
// its queue tasks hold. The caller still has to close the device.
func (d *Device) fail(err error) {
	if err == nil {
		return
	}

	d.invalid.CompareAndSwap(nil, &err)
	d.stopping.Store(true)
	d.cancel()
}
