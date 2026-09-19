package storage

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validManifest() ArtifactManifest {
	return ArtifactManifest{
		Version: ArtifactManifestVersion,
		BuildID: "build-1",
		Class:   ArtifactClassBuild,
		Entries: []ManifestEntry{
			{Path: "memfile", Size: 4096, Digest: strings.Repeat("ab", 32)},
			{Path: "memfile.header", Size: 512, Digest: strings.Repeat("cd", 32)},
		},
	}
}

func TestArtifactManifestRoundTrip(t *testing.T) {
	t.Parallel()

	m := validManifest()

	raw, err := MarshalArtifactManifest(m)
	require.NoError(t, err)

	got, err := ParseArtifactManifest(raw)
	require.NoError(t, err)
	require.Equal(t, m, got)
}

// A version or class this reader does not understand must never read as "no
// manifest": the caller has to skip and alert rather than treat the set as
// unreferenced (lane E's deletion-envelope pin P2).
func TestArtifactManifestUnknownVersionAndClass(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Version = ArtifactManifestVersion + 1

	raw, err := json.Marshal(m)
	require.NoError(t, err)

	_, err = ParseArtifactManifest(raw)
	require.ErrorIs(t, err, ErrManifestUnknownVersion)

	m = validManifest()
	m.Class = "snapshot"

	raw, err = json.Marshal(m)
	require.NoError(t, err)

	_, err = ParseArtifactManifest(raw)
	require.ErrorIs(t, err, ErrManifestUnknownClass)
}

// The manifest is deletion authority: a path that is not a plain relative path
// resolving inside the set, a negative size or a digest that is not a SHA-256
// makes the whole record untrustworthy (pin P1).
func TestArtifactManifestRefusesUntrustworthyEntries(t *testing.T) {
	t.Parallel()

	entries := map[string]ManifestEntry{
		"traversal":      {Path: "../other-build/memfile", Size: 1, Digest: strings.Repeat("ab", 32)},
		"absolute":       {Path: "/etc/passwd", Size: 1, Digest: strings.Repeat("ab", 32)},
		"empty path":     {Path: "", Size: 1, Digest: strings.Repeat("ab", 32)},
		"negative size":  {Path: "memfile", Size: -1, Digest: strings.Repeat("ab", 32)},
		"missing digest": {Path: "memfile", Size: 1, Digest: ""},
		"short digest":   {Path: "memfile", Size: 1, Digest: "abcd"},
		"non-hex digest": {Path: "memfile", Size: 1, Digest: strings.Repeat("zz", 32)},
	}

	for name, entry := range entries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := validManifest()
			m.Entries = []ManifestEntry{entry}

			_, err := MarshalArtifactManifest(m)
			require.ErrorIs(t, err, ErrManifestMalformed, "the writer refuses it too")

			raw, marshalErr := json.Marshal(m)
			require.NoError(t, marshalErr)

			_, err = ParseArtifactManifest(raw)
			require.ErrorIs(t, err, ErrManifestMalformed)
		})
	}

	// A syntactically invalid document is malformed, not absence.
	_, err := ParseArtifactManifest([]byte("{"))
	require.ErrorIs(t, err, ErrManifestMalformed)

	// An empty build id cannot anchor containment.
	m := validManifest()
	m.BuildID = ""
	m.Entries = nil

	_, err = MarshalArtifactManifest(m)
	require.ErrorIs(t, err, ErrManifestMalformed)
}

func TestArtifactManifestPath(t *testing.T) {
	t.Parallel()

	require.Equal(t, "build-1/manifest.json", ArtifactManifestPath(Paths{BuildID: "build-1"}))
}

// Two precision points from lane A's independent verification: the digest
// field is hex case-insensitively (the reader folds case), and an empty entry
// list parses as trusted — it names nothing to delete, and the candidacy rules
// still gate any action on the set.
func TestArtifactManifestDigestCaseAndEmptyEntries(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Entries[0].Digest = strings.ToUpper(m.Entries[0].Digest)

	raw, err := MarshalArtifactManifest(m)
	require.NoError(t, err)

	got, err := ParseArtifactManifest(raw)
	require.NoError(t, err)
	require.Equal(t, m.Entries, got.Entries, "an upper-case digest is the same digest")

	m = validManifest()
	m.Entries = nil

	raw, err = MarshalArtifactManifest(m)
	require.NoError(t, err)

	got, err = ParseArtifactManifest(raw)
	require.NoError(t, err)
	require.Empty(t, got.Entries)
}
