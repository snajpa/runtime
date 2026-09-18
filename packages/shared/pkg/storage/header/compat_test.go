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

// TestHeaderCapBoundary pins the 64→256 MiB cap semantics that stranded
// already-uploaded artifacts: an artifact above the historical cap must be
// accepted by the current reader, and the cap must still reject sizes above the
// current limit loudly.
//
//nolint:paralleltest // mutates the package-global cap; must not run in parallel
func TestHeaderCapBoundary(t *testing.T) {
	raw, err := os.ReadFile(compatFixtureFile("v4-header.bin"))
	require.NoError(t, err)

	artifactWithClaimedSize := func(size uint32) []byte {
		artifact := make([]byte, metadataSize+v4FlagsLen+v4SizePrefixLen+len("not-lz4!"))
		copy(artifact, raw[:metadataSize])
		artifact[metadataSize] = 0
		binary.LittleEndian.PutUint32(artifact[metadataSize+v4FlagsLen:], size)
		copy(artifact[metadataSize+v4FlagsLen+v4SizePrefixLen:], "not-lz4!")

		return artifact
	}

	t.Run("above the old 64 MiB cap is no longer rejected by the cap", func(t *testing.T) {
		_, err := DeserializeBytes(artifactWithClaimedSize(64<<20 + 1))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "exceeds cap",
			"the historical 64 MiB limit must not apply any more; the artifact may only fail later (decompression)")
	})

	t.Run("above the current cap is rejected loudly", func(t *testing.T) {
		_, err := DeserializeBytes(artifactWithClaimedSize(256<<20 + 1))
		require.ErrorContains(t, err, "exceeds cap")
	})

	t.Run("the historical cap rejected the same artifact", func(t *testing.T) {
		original := v4MaxUncompressedHeaderSize
		v4MaxUncompressedHeaderSize = 64 << 20
		t.Cleanup(func() { v4MaxUncompressedHeaderSize = original })

		_, err := DeserializeBytes(artifactWithClaimedSize(64<<20 + 1))
		require.ErrorContains(t, err, "exceeds cap",
			"with the historical cap restored the artifact is rejected on read — the exact incident class this suite pins")
	})
}
