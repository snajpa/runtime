//go:build linux

package nbd

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// writeCapture is one backend write as the dispatch path handed it over: the
// offset, the buffer's identity and capacity, a copy of the bytes, and the
// live slice, so a test can prove the backend's bytes were never scribbled on.
type writeCapture struct {
	off    int64
	length int
	cap    int
	ptr    *byte
	body   []byte
	live   []byte
}

// captureProv records writes for the dispatch write tests. hold, when set,
// keeps WriteAt inside the backend until the test closes it.
type captureProv struct {
	mu    sync.Mutex
	items []writeCapture

	wrote chan struct{}
	hold  chan struct{}
}

func (p *captureProv) ReadAt(_ context.Context, b []byte, _ int64) (int, error) { return len(b), nil }

func (p *captureProv) Size(_ context.Context) (int64, error) { return 1 << 40, nil }

func (p *captureProv) WriteZeroesAt(_, length int64) (int, error) { return int(length), nil }

func (p *captureProv) WriteAt(data []byte, off int64) (int, error) {
	w := writeCapture{
		off:    off,
		length: len(data),
		cap:    cap(data),
		body:   append([]byte(nil), data...),
		live:   data,
	}
	if len(data) > 0 {
		w.ptr = &data[0]
	}

	p.mu.Lock()
	p.items = append(p.items, w)
	p.mu.Unlock()

	select {
	case p.wrote <- struct{}{}:
	default:
	}

	if p.hold != nil {
		<-p.hold
	}

	return len(data), nil
}

func (p *captureProv) last(t *testing.T) writeCapture {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.items) == 0 {
		t.Fatal("the backend saw no write")
	}

	return p.items[len(p.items)-1]
}

// TestAcquireWriteDataPoolsTheBaseClass pins S-31: a body that fits the base
// class comes from the pool, and acquiring and releasing it allocates nothing.
//
//nolint:paralleltest // testing.AllocsPerRun panics in a parallel test.
func TestAcquireWriteDataPoolsTheBaseClass(t *testing.T) {
	// Not parallel: testing.AllocsPerRun requires the sequential test phase.
	allocs := testing.AllocsPerRun(100, func() {
		data, pooled := acquireWriteData(1 << 20)

		_ = data
		releaseWriteData(pooled)
	})
	if allocs != 0 {
		t.Fatalf("acquiring and releasing a pooled write buffer must not allocate, got %v allocations per run", allocs)
	}
}

// TestAcquireWriteDataAboveBaseClassAllocatesExactly pins that a request above
// the base class still allocates exactly its length and never lands in the
// pool: a sync.Pool can retain one buffer per P, so pooling the 32 MiB ceiling
// would put gigabytes of retention on a large node (INV-7).
func TestAcquireWriteDataAboveBaseClassAllocatesExactly(t *testing.T) {
	t.Parallel()

	length := uint32(dispatchBufferSize) + 4096

	data, pooled := acquireWriteData(length)
	if len(data) != int(length) || cap(data) != int(length) {
		t.Fatalf("buffer len/cap = %d/%d, want %d/%d (an exact allocation)", len(data), cap(data), length, length)
	}
	if pooled != nil {
		t.Fatal("a request above the base class must not take a pooled buffer")
	}

	releaseWriteData(pooled) // must be a safe no-op
}

// TestDispatchWriteServesThePooledBuffer pins S-31 end to end: a WRITE whose
// body spans two socket reads reaches the backend byte for byte, and its body
// comes from the pooled class rather than a per-request allocation.
func TestDispatchWriteServesThePooledBuffer(t *testing.T) {
	t.Parallel()

	const bodyLength = 1 << 20

	conn := &ctrlConn{
		reqCh:      make(chan []byte, 8),
		gate:       make(chan struct{}),
		firstWrite: make(chan struct{}),
	}
	prov := &captureProv{wrote: make(chan struct{}, 8)}
	d := NewDispatch(conn, prov, false)

	handleDone := make(chan struct{})
	go func() {
		_ = d.Handle(t.Context())
		close(handleDone)
	}()
	t.Cleanup(func() {
		select {
		case <-conn.gate:
		default:
			close(conn.gate)
		}
		close(conn.reqCh)
		<-handleDone
	})

	payload := bytes.Repeat([]byte{0x5A}, bodyLength)
	req := append(nbdRequest(NBDCmdWrite, 11, 0, bodyLength), payload...)

	// The header plus half the body in one read, the rest in a second: the
	// body spans the wait-for-more loop.
	conn.reqCh <- req[:28+bodyLength/2]
	conn.reqCh <- req[28+bodyLength/2:]

	select {
	case <-prov.wrote:
	case <-time.After(3 * time.Second):
		t.Fatal("the write never reached the backend")
	}

	w := prov.last(t)
	if w.off != 0 || w.length != bodyLength {
		t.Fatalf("backend saw offset %d length %d, want 0/%d", w.off, w.length, bodyLength)
	}
	if w.cap != dispatchBufferSize {
		t.Fatalf("write body capacity = %d, want %d: the body must come from the pooled class", w.cap, dispatchBufferSize)
	}
	if !bytes.Equal(w.body, payload) {
		t.Fatal("the backend did not receive the request payload")
	}
}

// TestDispatchWriteKeepsTheBufferUntilTheBackendReturns pins S-31's ownership
// rule: the body may only be returned to the pool after WriteAt has returned,
// even when the request loop gave up on ctx first. A buffer released early
// would be handed to the next request while the backend still writes from it.
func TestDispatchWriteKeepsTheBufferUntilTheBackendReturns(t *testing.T) {
	t.Parallel()

	const bodyLength = 512 << 10

	conn := &ctrlConn{
		reqCh:      make(chan []byte, 8),
		gate:       make(chan struct{}),
		firstWrite: make(chan struct{}),
	}
	prov := &captureProv{wrote: make(chan struct{}, 8), hold: make(chan struct{})}
	d := NewDispatch(conn, prov, false)

	ctx, cancel := context.WithCancel(t.Context())
	handleDone := make(chan struct{})
	go func() {
		_ = d.Handle(ctx)
		close(handleDone)
	}()
	t.Cleanup(func() {
		select {
		case <-prov.hold:
		default:
			close(prov.hold)
		}
		select {
		case <-conn.gate:
		default:
			close(conn.gate)
		}
		close(conn.reqCh)
		<-handleDone
	})

	payload := bytes.Repeat([]byte{0x42}, bodyLength)
	conn.reqCh <- append(nbdRequest(NBDCmdWrite, 13, 0, bodyLength), payload...)

	select {
	case <-prov.wrote:
	case <-time.After(3 * time.Second):
		t.Fatal("the write never reached the backend")
	}

	w := prov.last(t)

	// The request loop gives up while the backend is still inside WriteAt.
	cancel()

	// If the buffer were released before WriteAt returned, this acquire would
	// hand us the backend's memory.
	mine, pooled := acquireWriteData(bodyLength)
	if len(mine) > 0 && w.ptr != nil && &mine[0] == w.ptr {
		t.Fatal("the write buffer was released while the backend was still writing from it (S-31)")
	}

	for i := range mine {
		mine[i] = 0xEE
	}
	releaseWriteData(pooled)

	// Let the backend return; the bytes it was given must be intact.
	close(prov.hold)

	if !bytes.Equal(w.live, payload) {
		t.Fatal("the backend's bytes were corrupted while WriteAt still owned the buffer")
	}
}
