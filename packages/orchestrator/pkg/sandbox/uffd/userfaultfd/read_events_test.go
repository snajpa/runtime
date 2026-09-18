//go:build linux

package userfaultfd

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeUffdMsg builds one raw uffd message in the shape the kernel delivers:
// the event in msg.event and the payload in msg.arg (S-29).
func writeUffdMsg(t *testing.T, event CUChar, payload []byte) []byte {
	t.Helper()

	buf := make([]byte, unsafe.Sizeof(UffdMsg{}))
	msg := (*UffdMsg)(unsafe.Pointer(&buf[0]))
	msg.event = event
	require.LessOrEqual(t, len(payload), len(msg.arg))
	copy(msg.arg[:], payload)

	return buf
}

// TestAppendUffdMsgParsesValues: the decoder must return value slices whose
// contents survive the read buffer being overwritten (a batch no longer
// aliases the buffer).
func TestAppendUffdMsgParsesValues(t *testing.T) {
	t.Parallel()

	pf := UffdPagefault{flags: UFFD_PAGEFAULT_FLAG_WRITE, address: 0x1234}
	rem := UffdRemove{start: 0x1000, end: 0x2000}

	bufPF := writeUffdMsg(t, CUChar(UFFD_EVENT_PAGEFAULT), unsafe.Slice((*byte)(unsafe.Pointer(&pf)), unsafe.Sizeof(pf)))
	bufRM := writeUffdMsg(t, CUChar(UFFD_EVENT_REMOVE), unsafe.Slice((*byte)(unsafe.Pointer(&rem)), unsafe.Sizeof(rem)))

	removes, pagefaults, err := appendUffdMsg(nil, nil, bufPF)
	require.NoError(t, err)
	require.Empty(t, removes)
	require.Len(t, pagefaults, 1)
	assert.Equal(t, uintptr(0x1234), getPagefaultAddress(pagefaults[0]))
	assert.NotZero(t, pagefaults[0].flags&UFFD_PAGEFAULT_FLAG_WRITE)

	removes, pagefaults, err = appendUffdMsg(removes, pagefaults, bufRM)
	require.NoError(t, err)
	require.Len(t, removes, 1)
	assert.Equal(t, rem.start, removes[0].start)
	assert.Equal(t, rem.end, removes[0].end)

	// An unknown event type stays a hard error.
	_, _, err = appendUffdMsg(removes, pagefaults, writeUffdMsg(t, CUChar(0xFF), nil))
	require.ErrorIs(t, err, ErrUnexpectedEventType)
}

// TestAppendUffdMsgDoesNotAllocatePerEvent pins the S-29 goal: decoding into
// preallocated slices must not allocate per event (the old code heap-copied
// every event through a pointer).
//
//nolint:paralleltest // testing.AllocsPerRun panics in a parallel test.
func TestAppendUffdMsgDoesNotAllocatePerEvent(t *testing.T) {
	// Not parallel: testing.AllocsPerRun requires the sequential test phase.

	pf := UffdPagefault{address: 0x1234}
	buf := writeUffdMsg(t, CUChar(UFFD_EVENT_PAGEFAULT), unsafe.Slice((*byte)(unsafe.Pointer(&pf)), unsafe.Sizeof(pf)))

	removes := make([]UffdRemove, 0, 64)
	pagefaults := make([]UffdPagefault, 0, 64)

	var (
		lastErr error
		lastLen int
	)

	allocs := testing.AllocsPerRun(100, func() {
		removes, pagefaults, lastErr = appendUffdMsg(removes[:0], pagefaults[:0], buf)
		lastLen = len(pagefaults)
	})

	require.NoError(t, lastErr)
	require.Equal(t, 1, lastLen)
	assert.Zero(t, allocs, "decoding a message into preallocated slices must not allocate")
}
