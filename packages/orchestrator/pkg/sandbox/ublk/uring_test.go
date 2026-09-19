//go:build linux

package ublk

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The submission ring has to be sized for the commands a caller can have in
// flight between two flushes: overwriting a submission that has not been
// submitted yet would corrupt the ring silently, so it fails loudly instead.
func TestIOUringRejectsFullRing(t *testing.T) {
	t.Parallel()

	ring, err := newIOUring(2, 0, sqe64Size)
	if err != nil {
		t.Skipf("io_uring is not available: %v", err)
	}

	t.Cleanup(ring.close)

	// Two entries: two prepared SQEs fit, the third does not.
	ring.getSQE()
	ring.getSQE()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("preparing more SQEs than the ring holds did not panic")
		}

		if message, ok := recovered.(string); !ok || !strings.Contains(message, "submission ring is full") {
			t.Fatalf("unexpected panic value: %v", recovered)
		}
	}()

	ring.getSQE()
}

// io_uring_setup returns the lowest free descriptor, so with stdin closed it
// legitimately returns fd 0. The close predicate must not mistake that for the
// -1 sentinel: a ring on fd 0 still has to release it (R15).
//
//nolint:paralleltest // fd 0 must be briefly free and stdin put back; the test cannot share the process
func TestIOUringCloseClosesFDZero(t *testing.T) {
	// Free fd 0 for the ring and put stdin back afterwards. Not parallel: the
	// check needs fd 0 briefly free with no other test running.
	saved, err := unix.Dup(0)
	if err != nil {
		t.Skipf("cannot duplicate stdin: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Dup2(saved, 0)
		_ = unix.Close(saved)
	})

	if err := unix.Close(0); err != nil {
		t.Skipf("cannot close stdin: %v", err)
	}

	ring, err := newIOUring(2, 0, sqe64Size)
	if err != nil {
		t.Skipf("io_uring is not available: %v", err)
	}
	if ring.fd != 0 {
		ring.close()
		t.Skipf("io_uring_setup returned fd %d, not 0", ring.fd)
	}

	ring.close()

	if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("ring.close() left fd 0 open: %v", err)
	}
}
