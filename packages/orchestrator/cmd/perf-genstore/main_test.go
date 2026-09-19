package main

import "testing"

// The fixture classes are fixed by the design (fixtures.md): tiny = harness
// check, large = the M2-visible class, latency = per-artifact tails. A silent
// change of these shapes would invalidate every stored fingerprint.
func TestStoreClassesCoverTheFixedShapes(t *testing.T) {
	t.Parallel()

	want := map[string]struct {
		builds  int
		sizeMiB int
		kinds   string
	}{
		"tiny":    {32, 4, "rootfs.ext4,memfile"},
		"large":   {4, 512, "rootfs.ext4"},
		"latency": {64, 8, "rootfs.ext4,memfile"},
	}

	if len(storeClasses) != len(want) {
		t.Fatalf("classes = %d, want %d", len(storeClasses), len(want))
	}

	for name, w := range want {
		got, ok := storeClasses[name]
		if !ok {
			t.Fatalf("missing class %q", name)
		}

		if got.builds != w.builds || got.sizeMiB != w.sizeMiB || got.kinds != w.kinds {
			t.Fatalf("class %q = %+v, want %+v", name, got, w)
		}
	}
}
