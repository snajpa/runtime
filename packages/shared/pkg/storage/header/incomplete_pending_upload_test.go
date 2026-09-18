package header

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestStoreHeaderRefusesIncompleteHeader pins the invariant that an in-flight
// header never reaches object storage. The refusal happens before any storage
// access, so a nil provider is safe here.
func TestStoreHeaderRefusesIncompleteHeader(t *testing.T) {
	t.Parallel()

	h := &Header{
		Metadata:                &Metadata{Version: MetadataVersionV5, BlockSize: 4096, Size: 4096},
		IncompletePendingUpload: true,
	}

	cfg, stored, uncompressed, err := StoreHeader(t.Context(), nil, "builds/x/header", h)
	require.ErrorContains(t, err, "refusing to persist incomplete header")
	require.ErrorContains(t, err, "builds/x/header")
	require.Zero(t, cfg)
	require.Zero(t, stored)
	require.Zero(t, uncompressed)
}

// TestIncompletePendingUploadRoundTripV4V5 pins the documented format fact
// (header.go): the flag is bit 0 of the V4/V5 flags byte and is restored by
// the reader, so the state survives serialize -> deserialize.
func TestIncompletePendingUploadRoundTripV4V5(t *testing.T) {
	t.Parallel()

	buildID := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")

	for _, version := range []uint64{MetadataVersionV4, MetadataVersionV5} {
		for _, incomplete := range []bool{false, true} {
			h, err := NewHeader(
				&Metadata{Version: version, BlockSize: 4096, Size: 4096, BuildId: buildID, BaseBuildId: buildID},
				[]BuildMap{{Offset: 0, Length: 4096, BuildId: buildID}},
			)
			require.NoError(t, err)
			h.Builds = map[uuid.UUID]BuildData{buildID: {Size: 4096}}
			h.IncompletePendingUpload = incomplete

			data, err := SerializeHeader(h)
			require.NoError(t, err)

			got, err := DeserializeBytes(data)
			require.NoError(t, err)
			require.Equal(t, incomplete, got.IncompletePendingUpload,
				"version %d: the flag must survive the format round-trip", version)
		}
	}
}
