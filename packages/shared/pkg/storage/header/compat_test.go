package header

// S-55 — mixed-version compatibility & migration suite.
//
// Every header format version has a golden artifact under testdata/. The
// fixtures pin the on-disk format (the current writer must reproduce them byte
// for byte) and the reader matrix proves NFR-6/REQ-F2: new code reads old
// artifacts, unknown (future) versions are rejected loudly, and a
// format-affecting cap change must not strand stored headers (the 64→256 MiB
// incident).
//
// Regenerate deliberately with E2B_UPDATE_COMPAT_FIXTURES=1 (the run prints the
// new hashes; update the pins below in the same change).

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

const compatFixtureUpdateEnv = "E2B_UPDATE_COMPAT_FIXTURES"

type compatFixture struct {
	name       string
	version    uint64
	incomplete bool
	sha256     string
}

var compatFixtures = []compatFixture{
	{name: "v3-header.bin", version: 3, sha256: "d55e685cc764f1dcb8ab80bd7a30dfd5064ecfabf49e350212d6300d08bdc40d"},
	{name: "v4-header.bin", version: MetadataVersionV4, sha256: "95e6811af79e3807497a56a413f9ec2f95c1d921fd4e163c6c68faaa65241a16"},
	{name: "v5-header.bin", version: MetadataVersionV5, sha256: "96b3aec3f13e5ca729cdd8a3af90d89a0e75af7a6301cf4798d9792c803a7f81"},
	{name: "v5-incomplete-header.bin", version: MetadataVersionV5, incomplete: true, sha256: "ef3e7918cd3d244ecbdf72f7be20792d0d736092d91d080ff6cc68d7487ce480"},
}

func compatFixtureFile(name string) string {
	return filepath.Join("testdata", name)
}

// compatFixtureHeader builds the deterministic header each fixture encodes.
func compatFixtureHeader(t *testing.T, fixture compatFixture) *Header {
	t.Helper()

	buildA := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	buildB := uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002")

	const blockSize = uint64(4096)

	metadata := &Metadata{
		Version:     fixture.version,
		BlockSize:   blockSize,
		Size:        6 * blockSize,
		Generation:  1,
		BuildId:     buildA,
		BaseBuildId: buildB,
	}

	mappings := []BuildMap{
		{Offset: 0, Length: 2 * blockSize, BuildId: buildA, BuildStorageOffset: 0},
		{Offset: 2 * blockSize, Length: blockSize, BuildId: uuid.Nil},
		{Offset: 3 * blockSize, Length: blockSize, BuildId: buildB, BuildStorageOffset: 0},
		{Offset: 4 * blockSize, Length: 2 * blockSize, BuildId: buildA, BuildStorageOffset: 2 * blockSize},
	}

	h, err := NewHeader(metadata, mappings)
	require.NoError(t, err)

	h.IncompletePendingUpload = fixture.incomplete

	if metadataFormatVersion(fixture.version) >= MetadataVersionV4 {
		h.Builds = map[uuid.UUID]BuildData{
			buildA: {
				Size:     int64(4 * blockSize),
				Checksum: sha256.Sum256([]byte("compat-fixture-build-a")),
				FrameData: storage.NewFullFrameTable(storage.CompressionZstd, []storage.FrameSize{
					{U: int32(2 * blockSize), C: 1000},
					{U: int32(2 * blockSize), C: 1200},
				}).Table(),
			},
			buildB: {Size: int64(blockSize)},
		}
	}

	return h
}

// TestCompatibilityFixtureIntegrity freezes the on-disk format: the current
// writer must reproduce each checked-in artifact byte for byte. A failure here
// means a format change happened and must be deliberate (and versioned).
func TestCompatibilityFixtureIntegrity(t *testing.T) {
	t.Parallel()

	for _, fixture := range compatFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			data, err := SerializeHeader(compatFixtureHeader(t, fixture))
			require.NoError(t, err)

			sum := fmt.Sprintf("%x", sha256.Sum256(data))

			if os.Getenv(compatFixtureUpdateEnv) == "1" {
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				require.NoError(t, os.WriteFile(compatFixtureFile(fixture.name), data, 0o644))
				t.Logf("wrote %s (%d bytes) sha256=%s", fixture.name, len(data), sum)

				return
			}

			raw, err := os.ReadFile(compatFixtureFile(fixture.name))
			require.NoError(t, err, "fixture missing; regenerate with %s=1", compatFixtureUpdateEnv)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), sum,
				"the writer no longer reproduces the checked-in artifact; regenerate deliberately as a versioned format change")
			require.Equal(t, fixture.sha256, sum, "pinned fixture hash drifted")
		})
	}
}

// TestNewReaderReadsOldArtifacts is the new-reader/old-artifact matrix: every
// format version written by older builds must keep decoding with the current
// reader, preserving metadata, build map and mapping section.
func TestNewReaderReadsOldArtifacts(t *testing.T) {
	t.Parallel()

	for _, fixture := range compatFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(compatFixtureFile(fixture.name))
			require.NoError(t, err)

			got, err := DeserializeBytes(raw)
			require.NoError(t, err, "new readers must read older artifacts")

			want := compatFixtureHeader(t, fixture)

			require.Equal(t, want.Metadata.Version, got.Metadata.Version)
			require.Equal(t, want.Metadata.BlockSize, got.Metadata.BlockSize)
			require.Equal(t, want.Metadata.Size, got.Metadata.Size)
			require.Equal(t, want.Metadata.Generation, got.Metadata.Generation)
			require.Equal(t, want.Metadata.BuildId, got.Metadata.BuildId)
			require.Equal(t, want.Metadata.BaseBuildId, got.Metadata.BaseBuildId)
			require.Equal(t, want.IncompletePendingUpload, got.IncompletePendingUpload)
			require.True(t, Equal(want.Mapping.Slice(), got.Mapping.Slice()),
				"mapping sections must round-trip across versions")

			if metadataFormatVersion(fixture.version) < MetadataVersionV4 {
				// V3 artifacts carry no builds section: the raw decode leaves
				// Builds empty, and the storage-backed path (LoadHeader)
				// backfills an entry for every referenced uncompressed build.
				require.Empty(t, got.Builds)

				return
			}

			require.Len(t, got.Builds, len(want.Builds))

			for id, wantBuild := range want.Builds {
				gotBuild, ok := got.Builds[id]
				require.True(t, ok, "build %s missing from the decoded header", id)
				require.Equal(t, wantBuild.Size, gotBuild.Size)
				require.Equal(t, wantBuild.Checksum, gotBuild.Checksum)

				if wantBuild.FrameData == nil {
					require.Nil(t, gotBuild.FrameData)

					continue
				}

				require.NotNil(t, gotBuild.FrameData)
				require.Equal(t, wantBuild.FrameData.NumFrames(), gotBuild.FrameData.NumFrames())
			}
		})
	}
}

// TestLoudRejectionOfUnknownArtifacts covers the other half of NFR-6: bytes the
// current reader does not understand must fail loudly instead of being parsed
// into a plausible-but-wrong header.
func TestLoudRejectionOfUnknownArtifacts(t *testing.T) {
	t.Parallel()

	readFixture := func(t *testing.T, name string) []byte {
		t.Helper()

		raw, err := os.ReadFile(compatFixtureFile(name))
		require.NoError(t, err)

		return raw
	}

	t.Run("future format version is rejected by name", func(t *testing.T) {
		t.Parallel()

		future := append([]byte(nil), readFixture(t, "v5-header.bin")...)
		binary.LittleEndian.PutUint64(future[:8], MetadataVersionV5+1) // Metadata.Version leads the artifact

		_, err := DeserializeBytes(future)
		require.ErrorContains(t, err, fmt.Sprintf("unsupported header version %d", MetadataVersionV5+1))
	})

	t.Run("truncated artifacts error", func(t *testing.T) {
		t.Parallel()

		for _, fixture := range compatFixtures {
			raw := readFixture(t, fixture.name)

			_, err := DeserializeBytes(raw[:len(raw)/2])
			require.Error(t, err, "%s: half an artifact must not decode", fixture.name)
		}
	})

	t.Run("corrupted payload errors", func(t *testing.T) {
		t.Parallel()

		raw := readFixture(t, "v5-header.bin")
		corrupted := append([]byte(nil), raw...)

		// Flip bytes inside the LZ4 body: the artifact must be rejected, never
		// decoded into a partial header.
		for i := len(corrupted) - 8; i < len(corrupted); i++ {
			corrupted[i] ^= 0xFF
		}

		_, err := DeserializeBytes(corrupted)
		require.Error(t, err)
	})
}

// forgedClaimedSize rewrites the uncompressed-size prefix of a framed artifact
// (V4 and V5 share [metadata][flags][uint32 size][LZ4 block]) to the claimed
// size and replaces the body with bytes that cannot decompress, so an artifact
// that passes the cap guard fails later, loudly.
func forgedClaimedSize(t *testing.T, raw []byte, claimed uint32) []byte {
	t.Helper()

	artifact := make([]byte, metadataSize+v4FlagsLen+v4SizePrefixLen+len("not-lz4!"))
	copy(artifact, raw[:metadataSize])
	artifact[metadataSize] = 0
	binary.LittleEndian.PutUint32(artifact[metadataSize+v4FlagsLen:], claimed)
	copy(artifact[metadataSize+v4FlagsLen+v4SizePrefixLen:], "not-lz4!")

	return artifact
}

// TestHeaderCapBoundary pins the 64→256 MiB cap semantics that stranded
// already-uploaded artifacts: a block above the historical cap must not be
// rejected by the current per-format caps, above the current cap it must be
// rejected loudly, and the historical cap must be reproducible per format
// without mutating process state (S-41: caps are immutable and versioned).
func TestHeaderCapBoundary(t *testing.T) {
	t.Parallel()

	const historicalCap = 64 << 20

	t.Run("historical cap no longer applies to stored artifacts", func(t *testing.T) {
		t.Parallel()

		// 64 MiB + 1 was rejected while the cap was 64 MiB, stranding uploaded
		// snapshots; the current per-format caps must let it through so the
		// artifact may only fail later, in decompression.
		require.ErrorContains(t, checkUncompressedHeaderBlock("v4", historicalCap+1, historicalCap), "exceeds cap")
		require.ErrorContains(t, checkUncompressedHeaderBlock("v5", historicalCap+1, historicalCap), "exceeds cap")
		require.NoError(t, checkUncompressedHeaderBlock("v4", historicalCap+1, v4MaxUncompressedHeaderSize))
		require.NoError(t, checkUncompressedHeaderBlock("v5", historicalCap+1, v5MaxUncompressedHeaderSize))
	})

	for _, tc := range []struct {
		fixture string
		label   string
		cap     uint32
		read    func(*Metadata, []byte, int64) (*Header, error)
	}{
		{fixture: "v4-header.bin", label: "v4", cap: v4MaxUncompressedHeaderSize, read: deserializeV4WithCap},
		{fixture: "v5-header.bin", label: "v5", cap: v5MaxUncompressedHeaderSize, read: deserializeV5WithCap},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(compatFixtureFile(tc.fixture))
			require.NoError(t, err)

			metadata, err := deserializeMetadata(raw[:metadataSize])
			require.NoError(t, err)

			t.Run("above the current cap is rejected loudly", func(t *testing.T) {
				t.Parallel()

				_, err := DeserializeBytes(forgedClaimedSize(t, raw, tc.cap+1))
				require.ErrorContains(t, err, "exceeds cap")
			})

			t.Run("exactly at the cap passes the guard", func(t *testing.T) {
				t.Parallel()

				const size = uint32(64 << 10)
				block := forgedClaimedSize(t, raw, size)[metadataSize:]
				_, err := tc.read(metadata, block, int64(size))
				require.Error(t, err, "the forged body cannot decompress")
				require.NotContains(t, err.Error(), "exceeds cap", "a block exactly at the cap is legal")
			})
		})
	}
}
