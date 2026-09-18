package block

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrefetchTrackerTracksAndOrdersBlocks(t *testing.T) {
	t.Parallel()

	tracker := NewPrefetchTracker(4096)
	tracker.Add(0, Read)
	tracker.Add(4096, Write)
	tracker.Add(0, Read) // duplicate: keeps the first entry
	tracker.Add(8192, Prefetch)

	data := tracker.PrefetchData()
	require.Equal(t, int64(4096), data.BlockSize)
	require.Len(t, data.BlockEntries, 3)
	require.Equal(t, Read, data.BlockEntries[0].AccessType)
	require.Less(t, data.BlockEntries[0].Order, data.BlockEntries[1].Order)
	require.Less(t, data.BlockEntries[1].Order, data.BlockEntries[2].Order)
}

// A long-lived VM must not accumulate one entry per touched page: the window
// is bounded and the oldest blocks leave it first (REQ-D3/INV-7).
func TestPrefetchTrackerWindowIsBounded(t *testing.T) {
	t.Parallel()

	tracker := NewPrefetchTracker(4096)

	total := maxTrackedBlocks + 128
	for i := range total {
		tracker.Add(int64(i)*4096, Read)
	}

	data := tracker.PrefetchData()
	require.Len(t, data.BlockEntries, maxTrackedBlocks, "the window must stay bounded")
	require.NotContains(t, data.BlockEntries, uint64(0), "the oldest blocks are evicted")
	require.NotContains(t, data.BlockEntries, uint64(127))
	require.Contains(t, data.BlockEntries, uint64(total-1), "the newest blocks are kept")
}

// Collecting retires the window: tracking stops, the entries are released, and
// the snapshot is independent of later tracker state.
func TestPrefetchTrackerRetiresWindowOnCollect(t *testing.T) {
	t.Parallel()

	tracker := NewPrefetchTracker(4096)
	tracker.Add(0, Read)

	first := tracker.PrefetchData()
	require.Len(t, first.BlockEntries, 1)

	tracker.Add(4096, Read)
	second := tracker.PrefetchData()
	require.Empty(t, second.BlockEntries, "a retired window is not collected twice")
	require.Len(t, first.BlockEntries, 1, "the first snapshot stays intact")
}

func TestPrefetchTrackerResetStartsFreshWindow(t *testing.T) {
	t.Parallel()

	tracker := NewPrefetchTracker(4096)
	tracker.Add(0, Read)
	require.NotEmpty(t, tracker.PrefetchData().BlockEntries)

	tracker.Reset()
	tracker.Add(4096, Write)

	data := tracker.PrefetchData()
	require.Len(t, data.BlockEntries, 1)
	require.Contains(t, data.BlockEntries, uint64(1))
}

func TestPrefetchTrackerConcurrentAdds(t *testing.T) {
	t.Parallel()

	tracker := NewPrefetchTracker(4096)

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 512 {
				tracker.Add(int64(g*512+i)*4096, Read)
			}
		})
	}
	wg.Wait()

	data := tracker.PrefetchData()
	require.Len(t, data.BlockEntries, 8*512)
}
