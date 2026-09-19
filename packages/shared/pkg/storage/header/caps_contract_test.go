package header

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGuestInputCapsContract pins the header-side values of the S-52
// guest-input caps inventory. The enforcement behavior lives in the
// serialization and inspect tests; the literals are pinned here so that
// changing a cap fails loudly and forces the inventory, its consumers and the
// threat model to be revisited together.
//
// All three caps are consts (S-41 versioned them), so the read is safe to run
// in parallel with the rest of the package's tests.
func TestGuestInputCapsContract(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1<<20, maxVisualizeCells,
		"cells Visualize may allocate from untrusted sizes")
	require.Equal(t, 8<<20, maxV5MappingEntries,
		"V5 mapping entries accepted from a serialized header")
	require.Equal(t, 256<<20, v4MaxUncompressedHeaderSize,
		"uncompressed V4/V5 header block cap")
}
