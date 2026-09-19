package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyManifestEntries(t *testing.T) {
	t.Parallel()

	m := validManifest()

	// Everything the manifest names is where and what it claims.
	readings := map[string]ManifestEntryReading{
		"memfile":        {Size: 4096, Digest: strings.Repeat("ab", 32)},
		"memfile.header": {Size: 512, Digest: strings.Repeat("cd", 32)},
	}
	observe := func(e ManifestEntry) (ManifestEntryReading, error) { return readings[e.Path], nil }

	mismatches, err := VerifyManifestEntries(m, observe)
	require.NoError(t, err)
	require.Empty(t, mismatches)

	// A size or a digest that drifted is a mismatch, and the caller refuses the
	// set (pin P2's digest/size case).
	readings["memfile"] = ManifestEntryReading{Size: 4095, Digest: strings.Repeat("ab", 32)}

	mismatches, err = VerifyManifestEntries(m, observe)
	require.NoError(t, err)
	require.Len(t, mismatches, 1)
	require.Equal(t, "memfile", mismatches[0].Path)
	require.Contains(t, mismatches[0].Reason, "size")

	readings["memfile"] = ManifestEntryReading{Size: 4096, Digest: strings.Repeat("ee", 32)}

	mismatches, err = VerifyManifestEntries(m, observe)
	require.NoError(t, err)
	require.Len(t, mismatches, 1)
	require.Contains(t, mismatches[0].Reason, "SHA-256")

	// Mismatches accumulate per entry rather than stopping at the first, so a
	// set with two drifted objects reports both and the audit trail is
	// complete.
	readings["memfile"] = ManifestEntryReading{Size: 1, Digest: strings.Repeat("ee", 32)}
	readings["memfile.header"] = ManifestEntryReading{Size: 2, Digest: strings.Repeat("ff", 32)}

	mismatches, err = VerifyManifestEntries(m, observe)
	require.NoError(t, err)
	require.Len(t, mismatches, 2)
	require.ElementsMatch(t,
		[]string{"memfile", "memfile.header"},
		[]string{mismatches[0].Path, mismatches[1].Path},
	)

	// A provider that cannot read an object leaves the set unverified, which is
	// a refusal rather than a pass.
	_, err = VerifyManifestEntries(m, func(ManifestEntry) (ManifestEntryReading, error) {
		return ManifestEntryReading{}, errors.New("provider timeout")
	})
	require.ErrorIs(t, err, ErrManifestUnverified)

	_, err = VerifyManifestEntries(m, nil)
	require.ErrorIs(t, err, ErrManifestUnverified)
}
