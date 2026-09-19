//go:build linux

package ublk

import (
	"context"
	"errors"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

type zeroRange struct {
	off    int64
	length int64
}

// fakeBackend is an in-memory Backend. The read and write paths need a real
// char device and are covered by the integration test in the dev VM; the
// request mapping is testable without one.
type fakeBackend struct {
	mu       sync.Mutex
	data     []byte
	zeroes   []zeroRange
	readErr  error
	writeErr error
	zeroErr  error
}

func (b *fakeBackend) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.readErr != nil {
		return 0, b.readErr
	}
	if off < 0 || off+int64(len(p)) > int64(len(b.data)) {
		return 0, unix.EINVAL
	}

	return copy(p, b.data[off:]), nil
}

func (b *fakeBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.writeErr != nil {
		return 0, b.writeErr
	}
	if off < 0 || off+int64(len(p)) > int64(len(b.data)) {
		return 0, unix.EINVAL
	}

	return copy(b.data[off:], p), nil
}

func (b *fakeBackend) WriteZeroesAt(off, length int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.zeroErr != nil {
		return 0, b.zeroErr
	}
	if off < 0 || off+length > int64(len(b.data)) {
		return 0, unix.EINVAL
	}

	b.zeroes = append(b.zeroes, zeroRange{off: off, length: length})

	return int(length), nil
}

func (b *fakeBackend) Size(context.Context) (int64, error) { return int64(len(b.data)), nil }

func (b *fakeBackend) BlockSize() int64 { return 4096 }

// zeroCount reports how many zeroing requests the backend has seen.
func (b *fakeBackend) zeroCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.zeroes)
}

// setReadErr makes every later read fail, for the error-path tests.
func (b *fakeBackend) setReadErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.readErr = err
}

// slice returns a copy of the backend's data.
func (b *fakeBackend) slice(off, length int64) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]byte, length)
	copy(out, b.data[off:off+length])

	return out
}

// newTestQueue builds an owner whose descriptor array holds one request in tag
// 0, so handle can be exercised without a kernel device.
func newTestQueue(op uint8, nrSectors uint32, startSector uint64) (*queueOwner, *fakeBackend) {
	backend := &fakeBackend{data: make([]byte, 1<<20)}

	dev := &Device{backend: backend, opts: DefaultOptions(), size: int64(len(backend.data))}
	dev.ctx = context.Background()

	desc := make([]byte, ioDescSize*2)
	putU32(desc, ioDescOffOpFlags, uint32(op))
	putU32(desc, ioDescOffNrSectors, nrSectors)
	putU64(desc, ioDescOffStartSector, startSector)

	return &queueOwner{dev: dev, tags: []uint16{0, 1}, desc: desc}, backend
}

func TestHandleOps(t *testing.T) {
	t.Parallel()

	t.Run("discard and write zeroes go to the backend", func(t *testing.T) {
		t.Parallel()

		for _, op := range []uint8{ioOpDiscard, ioOpWriteZeroes} {
			owner, backend := newTestQueue(op, 8, 16)

			if got, want := owner.handle(0), int32(8*512); got != want {
				t.Errorf("op %d: result = %d, want %d", op, got, want)
			}

			want := zeroRange{off: 16 * 512, length: 8 * 512}
			if len(backend.zeroes) != 1 || backend.zeroes[0] != want {
				t.Errorf("op %d: zeroed %v, want %v", op, backend.zeroes, want)
			}
		}
	})

	t.Run("flush is a no-op", func(t *testing.T) {
		t.Parallel()

		owner, backend := newTestQueue(ioOpFlush, 8, 0)

		if got := owner.handle(0); got != 0 {
			t.Errorf("result = %d, want 0", got)
		}
		if len(backend.zeroes) != 0 {
			t.Errorf("flush touched the backend: %v", backend.zeroes)
		}
	})

	t.Run("unsupported ops fail", func(t *testing.T) {
		t.Parallel()

		const opWriteSame = 4

		owner, _ := newTestQueue(opWriteSame, 8, 0)

		if got, want := owner.handle(0), -int32(unix.EOPNOTSUPP); got != want {
			t.Errorf("result = %d, want %d", got, want)
		}
	})

	t.Run("requests outside the device fail", func(t *testing.T) {
		t.Parallel()

		owner, _ := newTestQueue(ioOpRead, 8, 1<<20)

		if got, want := owner.handle(0), -int32(unix.EIO); got != want {
			t.Errorf("result = %d, want %d", got, want)
		}
	})

	t.Run("backend errors become errnos", func(t *testing.T) {
		t.Parallel()

		owner, backend := newTestQueue(ioOpDiscard, 8, 0)
		backend.zeroErr = unix.ENOSPC

		if got, want := owner.handle(0), -int32(unix.ENOSPC); got != want {
			t.Errorf("result = %d, want %d", got, want)
		}
	})

	t.Run("a tag without a descriptor fails", func(t *testing.T) {
		t.Parallel()

		owner, _ := newTestQueue(ioOpRead, 8, 0)
		owner.desc = owner.desc[:ioDescSize]

		if got, want := owner.handle(1), -int32(unix.EIO); got != want {
			t.Errorf("result = %d, want %d", got, want)
		}
	})
}

// The commit carries the byte count on success, so the kernel derives the
// right residual for data-carrying requests like discard.
func TestHandleResults(t *testing.T) {
	t.Parallel()

	owner, _ := newTestQueue(ioOpDiscard, 1, 0)

	if got, want := owner.handle(0), int32(512); got != want {
		t.Errorf("result = %d, want %d", got, want)
	}
}

// U-10: in production these loops run against the kernel's device fd, where a
// retry or a short transfer is invisible; the injected syscall makes those
// paths deterministic here. The offset has to advance by exactly what each
// call consumed, so a short transfer never rewrites bytes it already filled.
func TestFullTransfersRetryAndAccumulate(t *testing.T) {
	t.Parallel()

	type step struct {
		n   int
		err error
	}
	cases := []struct {
		name    string
		buf     []byte
		steps   []step
		wantErr error
	}{
		{name: "one full transfer", buf: []byte("abcd"), steps: []step{{n: 4}}},
		{name: "short transfers accumulate", buf: []byte("abcd"), steps: []step{{n: 1}, {n: 2}, {n: 1}}},
		{name: "EINTR is retried", buf: []byte("abcd"), steps: []step{{err: unix.EINTR}, {n: 4}}},
		{name: "EINTR between short transfers", buf: []byte("abcd"), steps: []step{{n: 1}, {err: unix.EINTR}, {n: 3}}},
		{name: "a zero-length transfer is EIO", buf: []byte("abcd"), steps: []step{{n: 2}, {n: 0}}, wantErr: unix.EIO},
		{name: "a fatal error is returned", buf: []byte("abcd"), steps: []step{{err: unix.EPERM}}, wantErr: unix.EPERM},
		{name: "an empty buffer needs no transfer", buf: nil},
	}

	for _, direction := range []struct {
		name string
		call func(inject func(fd int, p []byte, off int64) (int, error), fd int, buf []byte, pos int64) error
	}{
		{"read", preadFullWith},
		{"write", pwriteFullWith},
	} {
		for _, tc := range cases {
			t.Run(direction.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				var (
					calls int
					offs  []int64
				)
				inject := func(_ int, _ []byte, off int64) (int, error) {
					calls++
					offs = append(offs, off)

					if calls > len(tc.steps) {
						t.Fatalf("call %d: unexpected extra transfer", calls)

						return 0, nil
					}

					return tc.steps[calls-1].n, tc.steps[calls-1].err
				}

				if err := direction.call(inject, 7, tc.buf, 100); !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}

				wantCalls := len(tc.steps)
				for i, s := range tc.steps {
					if (s.err != nil && !errors.Is(s.err, unix.EINTR)) || (s.err == nil && s.n == 0) {
						wantCalls = i + 1

						break
					}
				}
				if calls != wantCalls {
					t.Fatalf("transfers = %d, want %d", calls, wantCalls)
				}

				wantOff := int64(100)
				for i := range wantCalls {
					if offs[i] != wantOff {
						t.Fatalf("transfer %d at offset %d, want %d", i+1, offs[i], wantOff)
					}

					wantOff += int64(tc.steps[i].n)
				}
			})
		}
	}
}
