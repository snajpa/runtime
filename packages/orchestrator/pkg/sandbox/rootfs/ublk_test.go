//go:build linux

package rootfs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TestUblkProviderOverlayLifecycle drives the provider the way a sandbox does:
// start, write through the device, swap the cache out for a background seal,
// fold it back, export in place, and close. It is the ublk counterpart of the
// provider-level NBD tests and only runs where the ublk driver is.
func TestUblkProviderOverlayLifecycle(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("the ublk provider needs root and the ublk driver")
	}

	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skipf("ublk control device is not available: %v", err)
	}

	ctx := t.Context()

	const size = 64 << 20

	source, err := testutils.NewZeroDevice(size, header.RootfsBlockSize)
	require.NoError(t, err)

	featureFlags, err := featureflags.NewClient()
	require.NoError(t, err)
	t.Cleanup(func() { _ = featureFlags.Close(ctx) })

	cachePath := filepath.Join(t.TempDir(), "rootfs.cow")

	provider, err := NewUblkProvider(ctx, source, cachePath, featureFlags)
	require.NoError(t, err)

	require.NoError(t, provider.Start(ctx))

	node, err := provider.Path()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(node, "/dev/ublkb"), "device path %s is not a ublk device", node)

	device, err := os.OpenFile(node, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = device.Close() }()

	first := testPattern(0x41, 1<<20)

	_, err = device.WriteAt(first, 1<<20)
	require.NoError(t, err)
	require.NoError(t, device.Sync())

	// The seal path flushes the device and swaps the cache out as frozen, so
	// the write has to be in the frozen cache and not still in flight.
	frozen, err := provider.SwapForBackgroundSeal(ctx)
	require.NoError(t, err)
	require.NotNil(t, frozen)

	got := make([]byte, len(first))

	n, err := frozen.ReadAt(got, 1<<20)
	require.NoError(t, err)
	require.Equal(t, len(first), n)
	require.True(t, bytes.Equal(got, first), "the frozen cache does not hold the flushed write")

	// Writes after the swap go to the fresh cache. The frozen cache is a diff:
	// it holds what was dirty when it was frozen and nothing else, so the new
	// region is not in it at all.
	second := testPattern(0x42, 1<<20)

	_, err = device.WriteAt(second, 2<<20)
	require.NoError(t, err)
	require.NoError(t, device.Sync())

	got = make([]byte, len(second))

	_, err = frozen.ReadAt(got, 2<<20)

	var notAvailable block.BytesNotAvailableError
	require.ErrorAs(t, err, &notAvailable, "a write made after the swap reached the frozen cache")

	// Folding merges the sealing cache into the live one, which makes the live
	// cache a complete diff again and frees the sealing slot. The sealing cache
	// has to stay open until the fold has read it: the fold returns it detached
	// for closing, like the sandbox does after a background seal.
	detached, err := provider.FoldSealed(ctx)
	require.NoError(t, err)
	require.NotNil(t, detached)
	require.NoError(t, detached.Close())

	// An in-place export reads the live cache and has to see the write made
	// before it.
	out := filepath.Join(t.TempDir(), "diff.bin")

	diff, err := os.Create(out)
	require.NoError(t, err)
	defer diff.Close()

	meta, err := provider.ExportDiffInPlace(ctx, diff)
	require.NoError(t, err)
	require.NotNil(t, meta)

	info, err := diff.Stat()
	require.NoError(t, err)
	require.Positive(t, info.Size(), "the in-place export wrote no diff")

	// The sandbox keeps running on the overlay after the export.
	got = make([]byte, len(second))

	_, err = device.ReadAt(got, 2<<20)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, second), "the device stopped serving after the export")

	require.NoError(t, provider.Close(ctx))

	_, err = os.Stat(node)
	require.True(t, os.IsNotExist(err), "device node %s is still present after close", node)
}

// testPattern is a cheap, position-dependent pattern: a mismatch is visible
// without needing the source to hold real data.
func testPattern(seed byte, length int) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = byte(i)*3 + seed
	}

	return out
}
