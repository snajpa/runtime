//go:build linux

package ublk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// fileBackend serves a plain file. It stands in for the orchestrator's overlay
// when the point of the test is the device itself: what fio writes has to end
// up in this file, which the test can then read independently of the device.
type fileBackend struct {
	mu   sync.Mutex
	file *os.File
	size int64
}

func newFileBackend(t *testing.T, path string, size int64) *fileBackend {
	t.Helper()

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}

	if err := file.Truncate(size); err != nil {
		t.Fatalf("truncating %s: %v", path, err)
	}

	t.Cleanup(func() { _ = file.Close() })

	return &fileBackend{file: file, size: size}
}

func (b *fileBackend) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.file.ReadAt(p, off)
}

func (b *fileBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.file.WriteAt(p, off)
}

func (b *fileBackend) WriteZeroesAt(off, length int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Zero in chunks so a large discard does not allocate its whole range.
	const chunk = 1 << 20

	zeros := make([]byte, chunk)

	for done := int64(0); done < length; {
		n := min(length-done, chunk)

		written, err := b.file.WriteAt(zeros[:n], off+done)
		if err != nil {
			return int(done), err
		}

		done += int64(written)
	}

	return int(length), nil
}

func (b *fileBackend) Size(context.Context) (int64, error) { return b.size, nil }

func (b *fileBackend) BlockSize() int64 { return 4096 }

func (b *fileBackend) Sync() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.file.Sync()
}

// requireFio is what the block device has to survive: mixed direct and buffered
// I/O with fio's own verify step, driven by a different process than the one
// serving the device.
func requireFio(t *testing.T) string {
	t.Helper()

	path, err := exec.LookPath("fio")
	if err != nil {
		t.Skip("fio is not installed")
	}

	return path
}

// runFio runs one job file and returns its output.
func runFio(t *testing.T, fio, job string) ([]byte, error) {
	t.Helper()

	jobPath := filepath.Join(t.TempDir(), "fio.job")
	if err := os.WriteFile(jobPath, []byte(job), 0o600); err != nil {
		t.Fatalf("writing fio job: %v", err)
	}

	return exec.CommandContext(t.Context(), fio, "--output-format=normal", jobPath).CombinedOutput()
}

// TestFioDataIntegrity drives mixed I/O through the device with fio's
// verification on, then verifies the same regions a second time against the
// backend file with the device gone: a write the device acknowledged but never
// delivered fails the second pass.
//
// Every job owns a disjoint range. Two jobs overlapping one file with
// different verify layouts fail by construction, which says nothing about the
// device.
func TestFioDataIntegrity(t *testing.T) { //nolint:paralleltest // fio runs are I/O heavy: keep them serial on the VM
	mgr := requireUblk(t)
	fio := requireFio(t)
	ctx := t.Context()

	const size = 256 << 20

	backendPath := filepath.Join(t.TempDir(), "backend.img")
	backend := newFileBackend(t, backendPath, size)

	opts := DefaultOptions()
	opts.Queues = 2
	opts.MaxIOBufBytes = 1 << 20

	dev, err := mgr.Open(ctx, backend, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	closed := false

	defer func() {
		if !closed {
			if err := dev.Close(ctx); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	}()

	job := fmt.Sprintf(`[global]
ioengine=psync
direct=1
verify=crc32c
verify_async=2
group_reporting=1
filename=%s
[randwrite]
rw=randwrite
bs=4k
size=64m
numjobs=2
offset_increment=64m
[seqwrite]
rw=write
bs=1m
size=64m
offset=128m
`, dev.Path())

	out, err := runFio(t, fio, job)
	if err != nil {
		t.Fatalf("fio failed: %v\n%s", err, out)
	}

	t.Logf("fio write pass:\n%s", out)

	// The barrier has to make every acknowledged write visible in the backend.
	if err := dev.Sync(ctx); err != nil {
		t.Fatalf("device Sync: %v", err)
	}

	// Release the device before reading the backend: from here on the only
	// thing left to check is what the device delivered.
	if err := dev.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true

	if err := backend.Sync(); err != nil {
		t.Fatalf("backend sync: %v", err)
	}

	// Read the written ranges straight from the backend file and let fio verify
	// the blocks it wrote through the device.
	verifyJob := fmt.Sprintf(`[global]
ioengine=psync
direct=1
verify=crc32c
group_reporting=1
filename=%s
[verify-randwrite-0]
rw=read
bs=4k
size=64m
offset=0
[verify-randwrite-1]
rw=read
bs=4k
size=64m
offset=64m
[verify-seqwrite]
rw=read
bs=1m
size=64m
offset=128m
`, backendPath)

	out, err = runFio(t, fio, verifyJob)
	if err != nil {
		t.Fatalf("fio verify against the backend failed: %v\n%s", err, out)
	}
}

// TestFioFilesystem compares the device against a filesystem, which is where
// flush, discard and larger sequential I/O meet. It runs only when the host has
// the tools; the dev VM does.
func TestFioFilesystem(t *testing.T) { //nolint:paralleltest // fio runs are I/O heavy: keep them serial on the VM
	mgr := requireUblk(t)
	fio := requireFio(t)

	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 is not installed")
	}

	ctx := t.Context()

	backend := &fakeBackend{data: newPattern(0x99, 128<<20)}
	dev, err := mgr.Open(ctx, backend, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer func() {
		if err := dev.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	run := func(name string, args ...string) {
		t.Helper()

		out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run("mkfs.ext4", "-q", "-F", dev.Path())

	mountDir := t.TempDir()

	run("mount", dev.Path(), mountDir)
	defer func() {
		if out, err := exec.CommandContext(t.Context(), "umount", mountDir).CombinedOutput(); err != nil {
			t.Errorf("umount failed: %v\n%s", err, out)
		}
	}()

	// The guest-side flush path has to reach the backend: a filesystem sync
	// lands the data before the device is read back.
	out, err := exec.CommandContext(t.Context(), fio, "--name=fs", "--directory="+mountDir,
		"--ioengine=psync", "--rw=randrw", "--bs=4k", "--size=32m", "--verify=crc32c",
		"--direct=0", "--fsync_on_close=1").CombinedOutput()
	if err != nil {
		t.Fatalf("fio inside the filesystem failed: %v\n%s", err, out)
	}

	if err := dev.Sync(ctx); err != nil {
		t.Fatalf("device Sync: %v", err)
	}
}

// TestFioDirectWriteBackingFile keeps the write cache out of the picture: with
// direct I/O every byte fio writes has to reach the backend file itself.
func TestFioDirectWriteBackingFile(t *testing.T) { //nolint:paralleltest // fio runs are I/O heavy: keep them serial on the VM
	mgr := requireUblk(t)
	fio := requireFio(t)
	ctx := t.Context()

	const size = 64 << 20

	backendPath := filepath.Join(t.TempDir(), "direct.img")
	backend := newFileBackend(t, backendPath, size)

	dev, err := mgr.Open(ctx, backend, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer func() {
		if err := dev.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	out, err := exec.CommandContext(t.Context(), fio, "--name=direct", "--filename="+dev.Path(),
		"--ioengine=libaio", "--direct=1", "--rw=write", "--bs=128k", "--iodepth=16",
		"--size="+strconv.Itoa(size), "--verify=crc32c").CombinedOutput()
	if err != nil {
		t.Fatalf("fio failed: %v\n%s", err, out)
	}

	if err := dev.Sync(ctx); err != nil {
		t.Fatalf("device Sync: %v", err)
	}

	// The first megabyte has to agree between the backend file and the device,
	// byte for byte.
	fromBackend := make([]byte, 1<<20)
	if _, err := backend.ReadAt(ctx, fromBackend, 0); err != nil {
		t.Fatalf("reading the backend: %v", err)
	}

	node, err := os.OpenFile(dev.Path(), os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", dev.Path(), err)
	}
	defer node.Close()

	fromDevice := make([]byte, 1<<20)

	// Direct reads bypass the cache, so the backend is what is being read.
	if _, err := unix.Pread(int(node.Fd()), fromDevice, 0); err != nil {
		t.Fatalf("reading %s: %v", dev.Path(), err)
	}

	for i := range fromBackend {
		if fromBackend[i] != fromDevice[i] {
			t.Fatalf("backend and device disagree at offset %d", i)
		}
	}
}
