package storage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A manifest round-trips through a provider's own put/read path (the fs
// provider here), which is what makes the record durable before anything may
// treat it as deletion authority.
func TestArtifactManifestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()

	provider := newFileSystemStorage(t.TempDir(), "", nil)
	paths := Paths{BuildID: "build-1"}
	ctx := t.Context()

	m := validManifest()

	require.NoError(t, WriteArtifactManifest(ctx, provider, paths, m))

	got, err := ReadArtifactManifest(ctx, provider, paths)
	require.NoError(t, err)
	require.Equal(t, m, got)
}

// The writer refuses what the reader would have to skip, and nothing lands at
// the manifest path when it does.
func TestArtifactManifestWriteRefusesUntrustworthyRecords(t *testing.T) {
	t.Parallel()

	provider := newFileSystemStorage(t.TempDir(), "", nil)
	paths := Paths{BuildID: "build-1"}
	ctx := t.Context()

	m := validManifest()
	m.Entries = []ManifestEntry{{Path: "../other-build/memfile", Size: 1, Digest: strings.Repeat("ab", 32)}}

	require.ErrorIs(t, WriteArtifactManifest(ctx, provider, paths, m), ErrManifestMalformed)

	_, err := ReadArtifactManifest(ctx, provider, paths)
	require.ErrorIs(t, err, ErrObjectNotExist, "nothing should be readable at the manifest path")
}

// An absent manifest is reported as an error, never as an empty record: the
// reconciler must see "unproven", not "nothing owned".
func TestArtifactManifestReadMissing(t *testing.T) {
	t.Parallel()

	provider := newFileSystemStorage(t.TempDir(), "", nil)

	_, err := ReadArtifactManifest(t.Context(), provider, Paths{BuildID: "build-1"})
	require.ErrorIs(t, err, ErrObjectNotExist, "absence is the provider's not-exist error, never an empty record")
}

// A corrupt or foreign object at the manifest path is refused, and the cap
// keeps an oversized one from being buffered whole.
func TestArtifactManifestReadRefusesOversizedObject(t *testing.T) {
	t.Parallel()

	provider := newFileSystemStorage(t.TempDir(), "", nil)
	paths := Paths{BuildID: "build-1"}
	ctx := t.Context()

	blob, err := provider.OpenBlob(ctx, ArtifactManifestPath(paths))
	require.NoError(t, err)

	// Valid JSON that would parse as a manifest, so only the cap can refuse it:
	// lane A's bite showed the old malformed fixture passed with the cap removed.
	pad := strings.Repeat("a", maxArtifactManifestBytes+16)
	oversized := []byte(fmt.Sprintf(`{"version":%d,"build_id":"build-1","class":"build","entries":[],"padding":%q}`, ArtifactManifestVersion, pad))
	require.Greater(t, len(oversized), maxArtifactManifestBytes, "the fixture must exceed the cap")
	require.NoError(t, blob.Put(ctx, oversized))

	_, err = ReadArtifactManifest(ctx, provider, paths)
	require.ErrorIs(t, err, ErrManifestMalformed)
	require.ErrorContains(t, err, "exceeds")
}
