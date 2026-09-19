package main

import (
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func TestPlanRanges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		size  int64
		chunk int64
		want  []int64
	}{
		{"empty", 0, 1024, nil},
		{"zero chunk", 10, 0, nil},
		{"single", 512, 1024, []int64{0}},
		{"exact multiple", 2048, 1024, []int64{0, 1024}},
		{"remainder", 2500, 1024, []int64{0, 1024, 2048}},
	}

	for _, tc := range cases {
		got := planRanges(tc.size, tc.chunk)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}

		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

func TestPercentile(t *testing.T) {
	t.Parallel()

	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}

	durations := []time.Duration{10, 20, 30, 40, 50}

	if got := percentile(durations, 0); got != 10 {
		t.Errorf("p0 = %v, want 10", got)
	}

	if got := percentile(durations, 0.5); got != 30 {
		t.Errorf("p50 = %v, want 30", got)
	}

	if got := percentile(durations, 1); got != 50 {
		t.Errorf("p100 = %v, want 50", got)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	if got := humanBytes(4096); got != "4.0 KiB" {
		t.Errorf("humanBytes(4096) = %q", got)
	}

	if got := humanBytes(1 << 20); got != "1.0 MiB" {
		t.Errorf("humanBytes(1MiB) = %q", got)
	}
}

func TestAlignedWindow(t *testing.T) {
	t.Parallel()

	chunk := int64(storage.MemoryChunkSize)

	cases := []struct {
		name        string
		off, length int64
		wantStart   int64
		wantEnd     int64
	}{
		{"aligned start, short", 0, 1024, 0, chunk},
		{"aligned exact", 0, chunk, 0, chunk},
		{"unaligned start", 1, 512, 0, chunk},
		{"crosses boundary", chunk - 1, 2, 0, 2 * chunk},
		{"second window", chunk, 1024, chunk, 2 * chunk},
		{"unaligned, spans three", chunk + 7, chunk + 1, chunk, 3 * chunk},
	}

	for _, tc := range cases {
		gotStart, gotEnd := alignedWindow(tc.off, tc.length)
		if gotStart != tc.wantStart || gotEnd != tc.wantEnd {
			t.Errorf("%s: alignedWindow(%d, %d) = (%d, %d), want (%d, %d)",
				tc.name, tc.off, tc.length, gotStart, gotEnd, tc.wantStart, tc.wantEnd)
		}

		if gotStart > tc.off || gotEnd < tc.off+tc.length {
			t.Errorf("%s: window (%d, %d) does not enclose [%d, %d)", tc.name, gotStart, gotEnd, tc.off, tc.off+tc.length)
		}
	}
}
