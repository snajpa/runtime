package storage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// fuzzSeedFrameTable returns a small valid serialized frame table.
func fuzzSeedFrameTable(tb testing.TB) []byte {
	tb.Helper()

	ft := newFrameTableFromEntries(CompressionZstd, []frameEntry{
		{StartU: 0, StartC: 0, SizeU: 4096, SizeC: 2048},
		{StartU: 4096, StartC: 2048, SizeU: 4096, SizeC: 1024},
	})

	var buf bytes.Buffer

	if err := ft.Serialize(&buf); err != nil {
		tb.Fatalf("serialize seed frame table: %v", err)
	}

	return buf.Bytes()
}

// FuzzDeserializeFrameTable: frame tables come out of artifacts written by
// other nodes and older versions, so parsing must be total — a panic is the
// bug. Anything this package serializes must also deserialize back to a table
// with the same number of frames.
func FuzzDeserializeFrameTable(f *testing.F) {
	seed := fuzzSeedFrameTable(f)

	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		ft, err := DeserializeFrameTable(bytes.NewReader(data))
		if err != nil || ft == nil {
			return
		}

		var out bytes.Buffer
		require.NoError(t, ft.Serialize(&out), "a parsed frame table must serialize")

		ft2, err := DeserializeFrameTable(bytes.NewReader(out.Bytes()))
		require.NoError(t, err, "a frame table serialized by this package must deserialize")
		require.Equal(t, ft.NumFrames(), ft2.NumFrames())
	})
}
