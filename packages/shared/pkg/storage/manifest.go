package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ArtifactManifestVersion is the manifest shape this reader writes and is able
// to trust. The record is versioned because it becomes deletion authority: a
// version this reader does not understand must never read as "no manifest" —
// an older collector would then delete exactly the sets a newer writer just
// published — so ParseArtifactManifest returns a typed error, never a zero
// value, for anything it cannot fully trust.
const ArtifactManifestVersion = 1

// ArtifactManifestName is the file name a set's manifest is stored under inside
// its own storage directory.
const ArtifactManifestName = "manifest.json"

// Manifest parse errors are sentinels so callers can classify: everything this
// package cannot fully trust is skip + alert, never deletion proof.
var (
	ErrManifestUnknownVersion = errors.New("artifact manifest version this reader does not understand")
	ErrManifestUnknownClass   = errors.New("artifact manifest class this reader does not understand")
	ErrManifestMalformed      = errors.New("artifact manifest is malformed")
	ErrManifestUnverified     = errors.New("artifact manifest does not match the objects it names")
)

// ArtifactClass names the kind of artifact set a manifest describes; the
// reconciler keys its per-class policy (grace window, deletion caps) on it.
type ArtifactClass string

// ArtifactClassBuild is a build's artifact set: the per-build data files and
// their header sidecars. Intermediates without referential meaning (staged
// parts, temp files) are deliberately not manifest classes — age is their only
// sensible predicate, so bucket rules own those (S-37's recommended split).
const ArtifactClassBuild ArtifactClass = "build"

// ManifestEntry is one object a set owns: a path relative to the set's own
// storage directory, the object's size, and its whole-object SHA-256 in hex
// (any case: the reader folds it). Reclaim may delete only what an entry names,
// which is why the path's shape and containment and the digest are validated
// before the record is trusted at all.
type ManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// ArtifactManifest is S-37's additive, written-last record of what an artifact
// set consists of: the objects a set owns, so a lifecycle service can tell
// "still referenced by a snapshot" from "abandoned" without prefix guessing.
type ArtifactManifest struct {
	Version int             `json:"version"`
	BuildID string          `json:"build_id"`
	Class   ArtifactClass   `json:"class"`
	Entries []ManifestEntry `json:"entries"`
}

// ArtifactManifestPath returns the storage path of a set's manifest. Writing it
// is the last step of a publish: every object it lists is already durable, so a
// manifest never describes a set that is not there yet.
func ArtifactManifestPath(p Paths) string {
	return fmt.Sprintf("%s/%s", p.StorageDir(), ArtifactManifestName)
}

// MarshalArtifactManifest renders a manifest at the current version, refusing
// exactly what parse would refuse: a writer must not be able to publish a
// record its own readers would have to skip.
func MarshalArtifactManifest(m ArtifactManifest) ([]byte, error) {
	if err := validateArtifactManifest(m); err != nil {
		return nil, err
	}

	return json.Marshal(m)
}

// ParseArtifactManifest parses a manifest and refuses anything it cannot fully
// trust: an unknown version or class, an empty build id, an entry path that is
// not a plain relative path that resolves inside the set's own directory (the
// deletion-authority bypass the containment primitives exist to stop), a
// negative size, or a digest that is not a SHA-256. Every error means skip +
// alert for the caller: the set is left exactly as it is. An empty entry list
// is trusted — it names nothing to delete — but the set still has to be
// unreferenced, tombstoned and past grace before anything may happen to it.
func ParseArtifactManifest(raw []byte) (ArtifactManifest, error) {
	var m ArtifactManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ArtifactManifest{}, fmt.Errorf("%w: %w", ErrManifestMalformed, err)
	}

	if err := validateArtifactManifest(m); err != nil {
		return ArtifactManifest{}, err
	}

	return m, nil
}

func validateArtifactManifest(m ArtifactManifest) error {
	if m.Version != ArtifactManifestVersion {
		return fmt.Errorf("%w: %d", ErrManifestUnknownVersion, m.Version)
	}
	if m.Class != ArtifactClassBuild {
		return fmt.Errorf("%w: %q", ErrManifestUnknownClass, m.Class)
	}
	if m.BuildID == "" {
		return fmt.Errorf("%w: empty build id", ErrManifestMalformed)
	}

	for i, e := range m.Entries {
		if err := ValidateRelativePath(e.Path); err != nil {
			return fmt.Errorf("%w: entry %d path: %w", ErrManifestMalformed, i, err)
		}
		// The set's own directory is the manifest's directory, so an entry must
		// resolve under "." — the same containment rule the delete path applies.
		// This second check is defense-in-depth today: ValidateRelativePath
		// already rejects the traversal and absolute forms, and no known input
		// reaches this refusal. It is kept because the delete path runs the same
		// primitive, so the two cannot drift apart.
		if _, err := ContainedPath(".", e.Path); err != nil {
			return fmt.Errorf("%w: entry %d path escapes the set: %w", ErrManifestMalformed, i, err)
		}
		if e.Size < 0 {
			return fmt.Errorf("%w: entry %d has a negative size %d", ErrManifestMalformed, i, e.Size)
		}
		if !isSHA256Digest(e.Digest) {
			return fmt.Errorf("%w: entry %d has no SHA-256 digest", ErrManifestMalformed, i)
		}
	}

	return nil
}

func isSHA256Digest(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}

	_, err := hex.DecodeString(s)

	return err == nil
}
