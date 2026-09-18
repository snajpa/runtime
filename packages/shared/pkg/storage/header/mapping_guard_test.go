package header

import (
	"testing"
	"time"
)

// TestMergeMappingsTerminates guards the merge loop's progress. The old code
// printed to stderr and advanced no index on its invalid case, so an input that
// reached it spun forever. Adversarial (unsorted or oddly overlapping) inputs
// must now merge or return an error — and always return.
func TestMergeMappingsTerminates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		base []BuildMap
		diff []BuildMap
	}{
		{
			name: "diff inside base",
			base: []BuildMap{{Offset: 0, Length: 4 * blockSize, BuildId: baseID}},
			diff: []BuildMap{{Offset: blockSize, Length: 2 * blockSize, BuildId: diffID}},
		},
		{
			name: "unsorted base with partial overlap",
			base: []BuildMap{
				{Offset: 2 * blockSize, Length: 2 * blockSize, BuildId: baseID},
				{Offset: 0, Length: 2 * blockSize, BuildId: baseID},
			},
			diff: []BuildMap{{Offset: blockSize, Length: 2 * blockSize, BuildId: diffID}},
		},
		{
			name: "diff covers the whole base",
			base: []BuildMap{{Offset: 0, Length: 2 * blockSize, BuildId: baseID}},
			diff: []BuildMap{{Offset: 0, Length: 2 * blockSize, BuildId: diffID}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			done := make(chan struct{})

			go func() {
				defer close(done)

				_, _ = MergeMappings(tc.base, tc.diff)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("MergeMappings did not terminate on adversarial input")
			}
		})
	}
}
