//go:build linux

package nbd

import (
	"context"
	"testing"
)

// benchSinkProv is a minimal backend for the write-path benchmark: it signals
// completions without copying the body.
type benchSinkProv struct {
	done chan struct{}
}

func (p *benchSinkProv) ReadAt(_ context.Context, b []byte, _ int64) (int, error) { return len(b), nil }

func (p *benchSinkProv) Size(_ context.Context) (int64, error) { return 1 << 40, nil }

func (p *benchSinkProv) WriteZeroesAt(_, length int64) (int, error) { return int(length), nil }

func (p *benchSinkProv) WriteAt(data []byte, _ int64) (int, error) {
	select {
	case p.done <- struct{}{}:
	default:
	}

	return len(data), nil
}

// BenchmarkDispatchWriteServesOneRequest measures the dispatch write path with
// the request bytes built once, so the numbers are the path's allocations, not
// the benchmark's. Run it before and after the change: the body allocation it
// used to do per request is the S-31 claim.
func BenchmarkDispatchWriteServesOneRequest(b *testing.B) {
	const bodyLength = 1 << 20

	gate := make(chan struct{})
	close(gate)

	conn := &ctrlConn{
		reqCh:      make(chan []byte, 1),
		gate:       gate,
		firstWrite: make(chan struct{}),
	}
	prov := &benchSinkProv{done: make(chan struct{}, 1)}
	d := NewDispatch(conn, prov, false)

	ctx := b.Context()

	go func() {
		_ = d.Handle(ctx)
	}()

	req := append(nbdRequest(NBDCmdWrite, 1, 0, bodyLength), make([]byte, bodyLength)...)

	b.ReportAllocs()
	b.SetBytes(bodyLength)

	for b.Loop() {
		conn.reqCh <- req
		<-prov.done
	}
}
