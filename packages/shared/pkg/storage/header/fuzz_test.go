package header

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fuzzSeedHeader returns a small valid serialized header for the seed corpus.
func fuzzSeedHeader(tb testing.TB) []byte {
	tb.Helper()

	bs := uint64(4096)
	a, b := uuid.New(), uuid.New()
	mappings := []BuildMap{
		{Offset: 0, Length: bs, BuildId: a, BuildStorageOffset: 0},
		{Offset: bs, Length: bs, BuildId: b, BuildStorageOffset: 0},
	}
	meta := &Metadata{Version: MetadataVersionV4, BlockSize: bs, Size: 2 * bs, BuildId: a, BaseBuildId: b}

	h, err := NewHeader(meta, mappings)
	if err != nil {
		tb.Fatalf("build seed header: %v", err)
	}
	h.Builds = map[uuid.UUID]BuildData{a: {Size: int64(bs)}, b: {Size: int64(bs)}}

	data, err := SerializeHeader(h)
	if err != nil {
		tb.Fatalf("serialize seed header: %v", err)
	}

	return data
}

// FuzzDeserializeHeader: headers arrive from older versions, other nodes and
// potentially hostile storage, so parsing must be total — a panic is the bug.
// Additionally, any header this package serializes must deserialize again with
// the same shape.
func FuzzDeserializeHeader(f *testing.F) {
	seed := fuzzSeedHeader(f)

	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add(make([]byte, metadataSize))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := DeserializeBytes(data)
		if err != nil {
			return
		}

		if err := h.Mapping.Validate(h.Metadata.Size, PageSize); err != nil {
			return
		}

		out, err := SerializeHeader(h)
		if err != nil {
			return
		}

		h2, err := DeserializeBytes(out)
		require.NoError(t, err, "a header serialized by this package must deserialize")
		require.Equal(t, h.Metadata.Version, h2.Metadata.Version)
		require.Equal(t, h.Metadata.Size, h2.Metadata.Size)
		require.Equal(t, h.Mapping.Len(), h2.Mapping.Len())
	})
}

// FuzzDeserializeMetadata: the metadata block is fixed-size and read before any
// length validation of the rest, so a full block must always parse.
func FuzzDeserializeMetadata(f *testing.F) {
	f.Add(make([]byte, metadataSize))
	f.Add(make([]byte, metadataSize+1))
	f.Add([]byte{})
	f.Add([]byte{1, 2, 3})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < metadataSize {
			return
		}

		_, err := deserializeMetadata(data[:metadataSize])
		require.NoError(t, err, "a full metadata block must parse")
	})
}
