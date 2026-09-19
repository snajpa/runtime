package main

import (
	"testing"
	"time"
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
