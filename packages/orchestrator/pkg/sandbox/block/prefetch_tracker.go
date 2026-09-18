package block

import (
	"maps"
	"sync"
	"sync/atomic"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// PrefetchData contains block access data for prefetch mapping.
type PrefetchData struct {
	// BlockEntries contains metadata for each block index
	BlockEntries map[uint64]PrefetchBlockEntry
	// BlockSize is the size of each block in bytes
	BlockSize int64
}

// AccessType represents the type of access that caused a block to be loaded.
type AccessType string

const (
	// Read indicates a block loaded by a read operation.
	Read AccessType = "read"
	// Write indicates a block loaded by a write operation.
	Write AccessType = "write"
	// Prefetch indicates a proactively prefetched block, not a real fault.
	Prefetch AccessType = "prefetch"
)

// BlockEntry holds metadata about a tracked block.
type PrefetchBlockEntry struct {
	Index      uint64
	Order      uint64
	AccessType AccessType
}

// maxTrackedBlocks bounds the prefetch tracker's window: once the cap is
// reached the oldest tracked block leaves the window, so a long-lived VM can
// never accumulate one entry per touched page (REQ-D3/INV-7). 65536 entries
// cover 256 MiB of 4 KiB pages at a few MiB of bookkeeping.
const maxTrackedBlocks = 1 << 16

type PrefetchTracker struct {
	mu sync.RWMutex

	blockSize int64

	// blockEntries stores metadata for each block index
	blockEntries map[uint64]PrefetchBlockEntry
	// order lists tracked block indexes in insertion order; order[orderHead:]
	// is the live eviction queue.
	order     []uint64
	orderHead int
	// orderCounter tracks the next order number to assign
	orderCounter uint64

	isTracking atomic.Bool
}

func NewPrefetchTracker(blockSize int64) *PrefetchTracker {
	t := &PrefetchTracker{
		blockSize:    blockSize,
		blockEntries: make(map[uint64]PrefetchBlockEntry),
		orderCounter: 1,
	}
	t.isTracking.Store(true)

	return t
}

// Add adds an offset to the tracker with metadata about the access.
func (t *PrefetchTracker) Add(off int64, accessType AccessType) {
	if !t.isTracking.Load() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	idx := uint64(header.BlockIdx(off, t.blockSize))

	// Only add if not already tracked
	if _, ok := t.blockEntries[idx]; ok {
		return
	}

	// Keep the window bounded: the oldest tracked block leaves it at the cap.
	if len(t.blockEntries) >= maxTrackedBlocks {
		oldest := t.order[t.orderHead]
		t.orderHead++
		delete(t.blockEntries, oldest)

		if t.orderHead*2 >= len(t.order) {
			t.order = append(t.order[:0], t.order[t.orderHead:]...)
			t.orderHead = 0
		}
	}

	t.blockEntries[idx] = PrefetchBlockEntry{
		Index:      idx,
		Order:      t.orderCounter,
		AccessType: accessType,
	}
	t.order = append(t.order, idx)
	t.orderCounter++
}

// PrefetchData snapshots the tracked window and retires it: tracking stops and
// the collected entries are released, so a harvested tracker does not retain
// the VM's whole fault history. The returned snapshot is independent of the
// tracker; Reset starts a fresh window.
func (t *PrefetchTracker) PrefetchData() PrefetchData {
	t.isTracking.Store(false)

	t.mu.Lock()
	defer t.mu.Unlock()

	result := make(map[uint64]PrefetchBlockEntry, len(t.blockEntries))
	maps.Copy(result, t.blockEntries)

	t.blockEntries = make(map[uint64]PrefetchBlockEntry)
	t.order = nil
	t.orderHead = 0

	return PrefetchData{
		BlockEntries: result,
		BlockSize:    t.blockSize,
	}
}

// Reset clears the collected window and re-arms tracking, so a live tracker
// can start a fresh collection window instead of staying off permanently.
func (t *PrefetchTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.blockEntries = make(map[uint64]PrefetchBlockEntry)
	t.order = nil
	t.orderHead = 0
	t.isTracking.Store(true)
}
