package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGuestInputCapsContract pins the frame-table and codec-pool values of the
// S-52 guest-input caps inventory. The enforcement behavior lives in the frame
// table deserializer and in the codec pools' tests; the literals are pinned
// here so that changing a cap fails loudly and forces the inventory, its
// consumers and the threat model to be revisited together.
func TestGuestInputCapsContract(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1024*1024, maxDeserializedFrames,
		"frame-table entries read from a serialized object")
	require.Equal(t, 8, maxPooledCodecs,
		"codec instances each pool may retain")
}
