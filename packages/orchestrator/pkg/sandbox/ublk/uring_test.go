//go:build linux

package ublk

import (
	"strings"
	"testing"
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
