//go:build linux

package rootfs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGuestInputCapsContract pins the envd-swap values of the S-52 guest-input
// caps inventory. The enforcement behavior lives in the swap tests
// (envd_swap_linux_test.go, envd_swap_flow_linux_test.go); the literals are
// pinned here so that changing a cap fails loudly and forces the inventory, its
// consumers and the threat model to be revisited together.
func TestGuestInputCapsContract(t *testing.T) {
	t.Parallel()

	require.Equal(t, 256<<20, maxEnvdSize,
		"guest-controlled rootfs envd size the swap may dump onto the host")
	require.Equal(t, 64<<10, maxSwapOutput,
		"captured debugfs stdio bound")
	require.Equal(t, 2*time.Minute, EnvdSwapTimeout,
		"single debugfs invocation bound")
	require.Equal(t, 2*EnvdSwapTimeout, EnvdSwapBudget,
		"the mutating phases get two invocation bounds, not one")
}
