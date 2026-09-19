package storage

import (
	"fmt"
	"strings"
)

// ManifestEntryReading is what the store reports for one entry's object: its
// size and its whole-object SHA-256 in hex. A reader that cannot produce either
// returns an error — the set is then unverified, which is a refusal.
type ManifestEntryReading struct {
	Size   int64
	Digest string
}

// ManifestMismatch is one entry the store does not match.
type ManifestMismatch struct {
	Path   string
	Reason string
}

// VerifyManifestEntries compares every claim a manifest makes against what the
// store actually holds. Nothing here deletes: the caller turns a non-empty
// result — or an observation error — into skip + alert for the whole set,
// because a manifest that does not describe the objects it names must not
// authorise anything (lane E's P2 digest/size case).
func VerifyManifestEntries(m ArtifactManifest, observe func(ManifestEntry) (ManifestEntryReading, error)) ([]ManifestMismatch, error) {
	if observe == nil {
		return nil, fmt.Errorf("%w: no observer", ErrManifestUnverified)
	}

	var mismatches []ManifestMismatch

	for _, e := range m.Entries {
		got, err := observe(e)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %q: %w", ErrManifestUnverified, e.Path, err)
		}

		if got.Size != e.Size {
			mismatches = append(mismatches, ManifestMismatch{
				Path:   e.Path,
				Reason: fmt.Sprintf("size %d, manifest claims %d", got.Size, e.Size),
			})

			continue
		}

		if !strings.EqualFold(got.Digest, e.Digest) {
			mismatches = append(mismatches, ManifestMismatch{
				Path:   e.Path,
				Reason: "whole-object SHA-256 does not match the manifest",
			})
		}
	}

	return mismatches, nil
}
