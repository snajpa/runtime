//go:build linux

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGuestInputCapsContract pins the sandbox-side values of the S-52
// guest-input caps inventory (threat model + guest-input caps). The enforcement
// behavior is covered by the delivering items' tests (for this bound,
// envd_defaults_compare_test.go); the literal is pinned here so that changing
// the cap fails loudly and forces the inventory, its consumers and the threat
// model to be revisited together.
func TestGuestInputCapsContract(t *testing.T) {
	t.Parallel()

	require.Equal(t, 8<<10, envdDefaultsHeaderMaxBytes,
		"guest X-Envd-* header bytes are refused above this bound before decoding")
}
