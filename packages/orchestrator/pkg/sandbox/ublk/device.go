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

var (
	closeFD = unix.Close
	munmap  = unix.Munmap
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
	case o.MaxIOBufBytes%int(o.BlockSize) != 0:
		return o, fmt.Errorf("ublk: max I/O buffer size %d is not a multiple of block size %d", o.MaxIOBufBytes, o.BlockSize)
	case o.BlockSize != defaultBlockSize:
		return o, fmt.Errorf("ublk: first delivery requires block size %d, got %d", defaultBlockSize, o.BlockSize)
	}

	return o, nil
}

// OpenFailureKind explains whether a failed Open may safely fall back to
// another transport. Once the kernel has accepted ADD_DEV, fallback is never
// safe unless the caller receives a proven terminal result.
type OpenFailureKind uint8

const (
	OpenFailureNoSideEffect OpenFailureKind = iota
	OpenFailureQuarantined
)

// OpenError is returned by Manager.Open when the device could not become ready.
// For OpenFailureQuarantined, Open returns the non-nil Device as the ownership
// handle; the caller must retain it and must not fall back to another transport.
type OpenError struct {
	Kind OpenFailureKind
	Err  error
}

func (e *OpenError) Error() string {
	if e == nil || e.Err == nil {
		return "ublk: open failed"
	}

	return e.Err.Error()
}

func (e *OpenError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// FallbackSafe reports whether no kernel side effect was observed.
func (e *OpenError) FallbackSafe() bool {
	return e != nil && e.Kind == OpenFailureNoSideEffect
}

// QuarantineError means teardown did not prove that the kernel and all
// device-owned resources reached a terminal state. It is sticky: callers must
// retain the ownership unit and must not turn a later best-effort cleanup into
// clean success.
type QuarantineError struct {
	Err error
}

func (e *QuarantineError) Error() string {
	if e == nil || e.Err == nil {
		return "ublk: device quarantined"
	}

	return "ublk: device quarantined: " + e.Err.Error()
}

func (e *QuarantineError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// ErrActiveDevices means a Manager still owns a non-terminal Device.
var ErrActiveDevices = errors.New("ublk: manager has active devices")

// ErrDeviceClosing means a public operation raced terminal teardown.
var ErrDeviceClosing = errors.New("ublk: device is closing")

// Manager owns the control plane and creates devices. One Manager serves a
// process and is safe for concurrent use. It retains every non-terminal child.
type Manager struct {
	ctrl *controlPlane

	mu      sync.Mutex
	closed  bool
	devices map[*Device]struct{}
}

// NewManager opens /dev/ublk-control. It fails when the ublk driver is not
// loaded, which lets a caller fall back to another transport.
func NewManager() (*Manager, error) {
	ctrl, err := openControlPlane()
	if err != nil {
		return nil, err
	}

	return &Manager{ctrl: ctrl, devices: make(map[*Device]struct{})}, nil
}

// Close releases the control plane. Devices have to be closed first.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	if len(m.devices) != 0 {
		return ErrActiveDevices
	}
	m.closed = true
	m.ctrl.close()

	return nil
}

func (m *Manager) addDevice(d *Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return errors.New("ublk: manager is closed")
	}
	m.devices[d] = struct{}{}

	return nil
}

func (m *Manager) removeDevice(d *Device) {
	m.mu.Lock()
	delete(m.devices, d)
	m.mu.Unlock()
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

	addAttempted   atomic.Bool
	added          atomic.Bool
	started        atomic.Bool
	stopping       atomic.Bool
	abortRequested atomic.Bool
	inFlight       atomic.Int64
	invalid        atomic.Pointer[error]

	mu               sync.Mutex
	opMu             sync.Mutex
	state            deviceState
	releaseState     ReleaseState
	terminalErr      error
	terminalDone     chan struct{}
	terminalSignaled bool
	openDone         chan struct{}
}

// ReleaseState is the provider-visible resource outcome. It is independent of
// the sticky Close/Abort error: a failed caller result may eventually reach
// ReleaseProven, but it never becomes a successful Close/Abort result.
type ReleaseState uint8

const (
	ReleasePending ReleaseState = iota
	ReleaseProven
	ReleaseQuarantined
)

// deviceState is deliberately sticky: quarantined never becomes closed or
// successful later, even if a kernel command eventually returns.
type deviceState uint8

const (
	deviceStarting deviceState = iota
	deviceRunning
	deviceClosing
	deviceClosed
	deviceQuarantined
)

// Open creates the device, starts it and returns it with its node ready, so
// the path can go straight to Firecracker. Close deletes the device.
func (m *Manager) Open(ctx context.Context, backend Backend, opts Options) (*Device, error) {
	opts, err := opts.normalized()
	if err != nil {
		return nil, &OpenError{Kind: OpenFailureNoSideEffect, Err: err}
	}

	size, err := backend.Size(ctx)
	if err != nil {
		return nil, &OpenError{Kind: OpenFailureNoSideEffect, Err: fmt.Errorf("ublk: reading backend size: %w", err)}
	}
	if size <= 0 || size%opts.BlockSize != 0 {
		return nil, &OpenError{Kind: OpenFailureNoSideEffect, Err: fmt.Errorf("ublk: backend size %d is not a positive multiple of %d", size, opts.BlockSize)}
	}
	if backend.BlockSize() != opts.BlockSize {
		return nil, &OpenError{Kind: OpenFailureNoSideEffect, Err: fmt.Errorf("ublk: backend block size %d does not match requested %d", backend.BlockSize(), opts.BlockSize)}
	}

	d := &Device{
		mgr:          m,
		backend:      backend,
		opts:         opts,
		size:         size,
		cdevFD:       -1,
		blockFD:      -1,
		state:        deviceStarting,
		terminalDone: make(chan struct{}),
		openDone:     make(chan struct{}),
	}
	// The data path outlives the request that created the sandbox, so the
	// device context only ends with the device itself.
	d.ctx, d.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if err := m.addDevice(d); err != nil {
		return nil, &OpenError{Kind: OpenFailureNoSideEffect, Err: err}
	}

	openResult := make(chan error, 1)
	go func() {
		err := d.create()
		if err == nil {
			err = d.start()
		}
		d.finishOpen(err)
		openResult <- err
	}()

	select {
	case err := <-openResult:
		if err != nil {
			return d.openFailure(err)
		}

		return d, nil
	case <-ctx.Done():
		err := fmt.Errorf("ublk: opening device did not complete: %w", ctx.Err())
		d.recordFailure(err)

		return d, &OpenError{Kind: OpenFailureQuarantined, Err: err}
	}
}

func (d *Device) finishOpen(err error) {
	d.mu.Lock()
	if err == nil && d.state == deviceStarting {
		d.state = deviceRunning
	}
	close(d.openDone)
	d.mu.Unlock()
}

func (d *Device) openFailure(err error) (*Device, error) {
	if d.addAttempted.Load() || d.added.Load() {
		d.recordFailure(err)

		return d, &OpenError{Kind: OpenFailureQuarantined, Err: err}
	}
	d.mgr.removeDevice(d)

	return nil, &OpenError{Kind: OpenFailureNoSideEffect, Err: err}
}

// Path is the block device node the device is exported as.
func (d *Device) Path() string { return d.path }

// ID is the device number the kernel assigned.
func (d *Device) ID() uint32 { return d.id }

// Options returns the device's effective options.
func (d *Device) Options() Options { return d.opts }

// ReleaseState reports whether provider-owned resources are still pending,
// safely releasable, or quarantined. It never clears a sticky Close/Abort
// error.
func (d *Device) ReleaseState() ReleaseState {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.releaseState
}

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

	// An ioctl/uring command error does not prove that the kernel made no
	// change. Mark the attempt before submission so Open retains ownership on
	// every uncertain ADD_DEV result.
	d.addAttempted.Store(true)
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
	d.opMu.Lock()
	defer d.opMu.Unlock()

	return d.syncLocked(ctx, false)
}

func (d *Device) syncLocked(ctx context.Context, allowClosing bool) error {
	d.mu.Lock()
	state := d.state
	terminalErr := d.terminalErr
	started := d.started.Load()
	fd := d.blockFD
	path := d.path
	d.mu.Unlock()

	if state == deviceClosing && !allowClosing {
		return ErrDeviceClosing
	}
	if state == deviceQuarantined || (state == deviceClosed && terminalErr != nil) {
		if terminalErr != nil {
			return terminalErr
		}
		return &QuarantineError{Err: errors.New("terminal device result is not clean")}
	}

	if err := d.Failure(); err != nil {
		return err
	}

	if !started || fd < 0 {
		return nil
	}

	// The descriptor opened at start is the one whose mapping records
	// writeback errors, so it is the one to sync, like the NBD transport's.
	syncErr := unix.Fsync(fd)

	// Invalidate even when the sync failed: the device is about to be exported
	// or reused, and stale pages must not outlive the error.
	invalidateErr := unix.IoctlSetInt(fd, unix.BLKFLSBUF, 0)

	if err := d.waitInFlight(ctx); err != nil {
		return err
	}

	if err := d.Failure(); err != nil {
		return err
	}

	var errs []error
	if syncErr != nil {
		errs = append(errs, fmt.Errorf("ublk: syncing %s: %w", path, syncErr))
	}
	if invalidateErr != nil {
		errs = append(errs, fmt.Errorf("ublk: invalidating %s: %w", path, invalidateErr))
	}

	return errors.Join(errs...)
}

// Close syncs, stops and deletes the device. Cleanup is owned by one worker.
// A caller deadline returns a sticky quarantine result while that worker may
// still be blocked in an uninterruptible kernel control command.
func (d *Device) Close(ctx context.Context) error {
	return d.terminate(ctx, true, nil)
}

// Abort stops a device after a failed barrier or backend failure. It preserves
// the cause and never reports clean export/release success.
func (d *Device) Abort(ctx context.Context, cause error) error {
	return d.terminate(ctx, false, cause)
}

func (d *Device) terminate(ctx context.Context, clean bool, cause error) error {
	d.mu.Lock()
	var done <-chan struct{}
	switch d.state {
	case deviceClosed, deviceQuarantined:
		err := d.terminalErr
		d.mu.Unlock()
		return err
	case deviceClosing:
		done = d.terminalDone
		d.mu.Unlock()
	default:
		d.state = deviceClosing
		done = d.terminalDone
		d.mu.Unlock()
		go d.runTerminal(clean, cause)
	}

	return d.waitTerminal(ctx, done)
}

func (d *Device) waitTerminal(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return d.terminalResult()
	case <-ctx.Done():
		d.markQuarantined(fmt.Errorf("ublk: terminal cleanup did not complete: %w", ctx.Err()))
		return d.terminalResult()
	}
}

func (d *Device) runTerminal(clean bool, cause error) {
	if d.openDone != nil {
		<-d.openDone
	}
	if cause != nil {
		d.recordFailure(cause)
	}

	// Do not inherit the caller's deadline: caller cancellation changes the
	// caller-visible outcome to quarantine, but cannot safely interrupt STOP or
	// DEL_DEV_ASYNC.
	err, quarantined := d.teardown(context.Background(), clean)
	if cause != nil {
		err = errors.Join(cause, err)
	}
	if failure := d.Failure(); failure != nil && !errors.Is(err, failure) {
		err = errors.Join(failure, err)
	}

	d.mu.Lock()
	if quarantined {
		d.releaseState = ReleaseQuarantined
	} else {
		d.releaseState = ReleaseProven
		// A prior caller timeout leaves the terminal error quarantined, but a
		// later completed worker still proves resource release.
		d.mgr.removeDevice(d)
	}
	if d.state != deviceQuarantined {
		d.terminalErr = err
		if quarantined {
			d.state = deviceQuarantined
		} else {
			d.state = deviceClosed
		}
		if !d.terminalSignaled {
			d.terminalSignaled = true
			close(d.terminalDone)
		}
	}
	d.mu.Unlock()
}

func (d *Device) terminalResult() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.terminalErr
}

func (d *Device) markQuarantined(cause error) {
	if cause == nil {
		cause = errors.New("terminal cleanup did not complete")
	}
	if failure := d.Failure(); failure != nil && !errors.Is(cause, failure) {
		cause = errors.Join(failure, cause)
	}
	d.recordFailure(cause)

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == deviceClosed || d.state == deviceQuarantined {
		return
	}
	d.state = deviceQuarantined
	d.terminalErr = &QuarantineError{Err: cause}
	if !d.terminalSignaled {
		d.terminalSignaled = true
		close(d.terminalDone)
	}
}

// teardown takes the device down in the order the kernel expects. It returns a
// second boolean because an error is not enough to tell whether releasing the
// backend is safe: uncertain control/owner completion is quarantine.
func (d *Device) teardown(ctx context.Context, barrier bool) (error, bool) {
	d.opMu.Lock()
	defer d.opMu.Unlock()

	var errs []error
	var releaseErrs []error

	if barrier && d.started.Load() && !d.abortRequested.Load() {
		if err := d.syncLocked(ctx, true); err != nil {
			errs = append(errs, err)
			d.abortRequested.Store(true)
			d.cancel()
		}
	} else if !barrier {
		d.abortRequested.Store(true)
		d.cancel()
	}

	// Release the device node before stopping. The provider must have already
	// detached Firecracker; after this call no new opener is admitted here.
	if err := d.closeBlockDevice(); err != nil {
		errs = append(errs, err)
		releaseErrs = append(releaseErrs, err)
	}

	if d.added.Load() {
		if err := d.mgr.command(ublkCmdStopDev, ctrlCmd{devID: d.id, queueID: queueIDNone}); err != nil && !errors.Is(err, unix.ENODEV) {
			err := errors.Join(append(errs, fmt.Errorf("ublk: stopping device %d: %w", d.id, err))...)
			return &QuarantineError{Err: err}, true
		}
	}

	// STOP has returned, so owners may now observe stopping and exit. A
	// non-context write can still keep one inside the backend; that is retained.
	d.stopping.Store(true)
	if !d.waitOwners(ownerExitGrace) {
		d.cancel()
		if !d.waitOwners(ownerExitGrace) {
			err := errors.Join(append(errs, fmt.Errorf("ublk: queue tasks of device %d did not exit", d.id))...)
			return &QuarantineError{Err: err}, true
		}
	}

	// The command is synchronous from this API's perspective: even ASYNC delete
	// may take the kernel's global control mutex and stop path. Do not release
	// resources merely because the command was submitted.
	if err := d.delete(); err != nil {
		err := errors.Join(append(errs, err)...)
		return &QuarantineError{Err: err}, true
	}
	if d.path != "" {
		if err := waitNodeGone(d.path, deviceNodeTimeout); err != nil {
			err := errors.Join(append(errs, err)...)
			return &QuarantineError{Err: err}, true
		}
	}

	for _, owner := range d.owners {
		if err := owner.ring.closeChecked(); err != nil {
			err = fmt.Errorf("ublk: closing queue %d ring: %w", owner.qid, err)
			errs = append(errs, err)
			releaseErrs = append(releaseErrs, err)
		}
	}
	remainingBufs := d.queueBufs[:0]
	for _, buf := range d.queueBufs {
		if err := munmap(buf); err != nil {
			err = fmt.Errorf("ublk: unmapping queue buffer: %w", err)
			errs = append(errs, err)
			releaseErrs = append(releaseErrs, err)
			remainingBufs = append(remainingBufs, buf)
		}
	}
	d.queueBufs = remainingBufs
	if err := d.closeCdev(); err != nil {
		errs = append(errs, err)
		releaseErrs = append(releaseErrs, err)
	}

	return errors.Join(errs...), len(releaseErrs) != 0
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

	if errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("ublk: deleting device %d: async delete is unsupported", d.id)
	}

	return fmt.Errorf("ublk: deleting device %d: %w", d.id, err)
}

func (d *Device) closeBlockDevice() error {
	if d.blockFD < 0 {
		return nil
	}

	err := closeFD(d.blockFD)
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

	err := closeFD(d.cdevFD)
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
func (d *Device) recordFailure(err error) {
	if err == nil {
		return
	}

	d.invalid.CompareAndSwap(nil, &err)
	d.abortRequested.Store(true)
	d.cancel()
}

func (d *Device) fail(err error) {
	d.recordFailure(err)
}
