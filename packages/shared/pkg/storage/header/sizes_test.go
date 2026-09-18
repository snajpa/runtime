package header

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSizeConventions pins the invariants of the size constants this package
// exports (aliases of the canonical definitions in packages/shared/pkg/units).
func TestSizeConventions(t *testing.T) {
	t.Parallel()

	// EmptyHugePage is sized from HugepageSize; a drift would mis-size every
	// huge-page comparison.
	assert.Len(t, EmptyHugePage, HugepageSize)

	// The page must divide the larger conventions, or a page-granular read
	// inside one frame/hugepage could span two of them.
	assert.Zero(t, HugepageSize%PageSize)
	assert.Zero(t, RootfsBlockSize%PageSize)
}
