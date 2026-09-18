package storage

// Codec pools reuse expensive per-frame buffers: a zstd decoder retains its
// window and decode buffers, an LZ4 writer keeps a hash table. sync.Pool has no
// size limit and no way to observe or bound what it retains, so under a load
// spike it can hold an unbounded number of codecs until a GC happens to drop
// them (INV-7). These pools are bounded instead: an instance offered to a full
// pool is discarded (closed where the codec owns resources) rather than kept.

// maxPooledCodecs caps how many codec instances each pool retains. It is small
// on purpose — the cap exists to bound retained codec buffers, and a spike can
// always construct more instances on demand.
const maxPooledCodecs = 8

// codecPool is a bounded free list of reusable codec instances.
type codecPool[T any] struct {
	ch      chan T
	discard func(T)
}

func newCodecPool[T any](capacity int, discard func(T)) *codecPool[T] {
	return &codecPool[T]{
		ch:      make(chan T, capacity),
		discard: discard,
	}
}

// get returns a pooled instance when one is available.
func (p *codecPool[T]) get() (T, bool) {
	select {
	case v := <-p.ch:
		return v, true
	default:
		var zero T

		return zero, false
	}
}

// put offers v to the pool; a full pool discards it instead of retaining it.
func (p *codecPool[T]) put(v T) {
	select {
	case p.ch <- v:
	default:
		if p.discard != nil {
			p.discard(v)
		}
	}
}

// len reports how many instances are currently pooled.
func (p *codecPool[T]) len() int {
	return len(p.ch)
}
