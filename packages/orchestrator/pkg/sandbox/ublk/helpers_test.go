//go:build linux

package ublk

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireUblk(t *testing.T) *Manager {
	t.Helper()
	if os.Getenv("E2B_RUN_UBLK_LIVE") != "1" {
		t.Skip("set E2B_RUN_UBLK_LIVE=1 for the explicit live ublk lane")
	}

	mgr, err := NewManager()
	if err != nil {
		t.Skipf("ublk control device unavailable: %v", err)
	}
	t.Cleanup(func() { require.NoError(t, mgr.Close()) })

	return mgr
}
