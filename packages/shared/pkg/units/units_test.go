package units

import "testing"

func TestUnitDefinitions(t *testing.T) {
	t.Parallel()

	if KiB != 1<<10 {
		t.Errorf("KiB = %d, want %d", KiB, 1<<10)
	}
	if MiB != 1<<20 {
		t.Errorf("MiB = %d, want %d", MiB, 1<<20)
	}
	if MiB%KiB != 0 {
		t.Errorf("MiB %% KiB = %d, want 0", MiB%KiB)
	}
}

// TestSizeDerivations turns the conventions that used to live only in comments
// (storage.go's "bigger or equal to the block size", compress_config.go's
// "MUST be multiple of every block/page size") into test failures on drift.
func TestSizeDerivations(t *testing.T) {
	t.Parallel()

	if RootfsBlockSize != PageSize {
		t.Errorf("RootfsBlockSize = %d, want PageSize (%d)", RootfsBlockSize, PageSize)
	}

	pairs := []struct {
		name  string
		total int
		unit  int
	}{
		{"MemoryChunkSize must cover a hugepage", MemoryChunkSize, HugepageSize},
		{"MemoryChunkSize must cover a page", MemoryChunkSize, PageSize},
		{"HugepageSize must cover a page", HugepageSize, PageSize},
		{"DefaultCompressFrameSize must cover a hugepage", DefaultCompressFrameSize, HugepageSize},
		{"DefaultCompressFrameSize must cover the rootfs block", DefaultCompressFrameSize, RootfsBlockSize},
	}
	for _, p := range pairs {
		if p.total < p.unit {
			t.Errorf("%s: %d < %d", p.name, p.total, p.unit)
		}
		if p.total%p.unit != 0 {
			t.Errorf("%s: %d is not a multiple of %d", p.name, p.total, p.unit)
		}
	}
}

func TestMBConversions(t *testing.T) {
	t.Parallel()

	if got := MBToBytes(3); got != 3*MiB {
		t.Errorf("MBToBytes(3) = %d, want %d", got, 3*MiB)
	}
	if got := BytesToMB(3 * MiB); got != 3 {
		t.Errorf("BytesToMB(%d) = %d, want 3", 3*MiB, got)
	}
}
