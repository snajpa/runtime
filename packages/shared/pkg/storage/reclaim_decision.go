package storage

import "fmt"

// ReclaimDecision is the lifecycle service's per-set verdict. False is the
// common case: the reconciler removes only what it can prove abandoned, so a
// missing or unreadable input is a refusal, never an assumption.
type ReclaimDecision struct {
	Candidate bool
	Reason    string
}

// ReclaimInput is everything the decision is allowed to consider — S-37's
// deletion prerequisites R4-R6. A caller that cannot supply a field leaves it
// false: eligibility must be established, never assumed.
type ReclaimInput struct {
	// ManifestErr is the manifest read/parse outcome. Any error means skip and
	// alert (lane E's P2): an unknown version or an unreadable record is never
	// read as "no manifest". An absent manifest must arrive as the provider's
	// not-exist error — `ReadArtifactManifest` reports it that way — because
	// nil here means a manifest was read and parsed; the decision cannot tell
	// an absent manifest from a fine one, so a caller must never pass nil for
	// absence (lane A's verification note).
	ManifestErr error

	// Referenced is true when a live descendant still needs this set (R4);
	// ReferenceKnown is false when the reference graph could not be read — a
	// legacy V3 descendant without a Builds map, or a scan that failed part-way.
	Referenced     bool
	ReferenceKnown bool

	// PublishedBeforeScan is false for a set whose manifest was written after
	// the candidate scan began: an in-flight publish is never swept.
	PublishedBeforeScan bool

	// Tombstoned is the authorisation: the set's deletion has been requested.
	// It is not a revocation barrier for readers (R6).
	Tombstoned bool

	// GraceElapsed is true once the declared grace window has passed.
	GraceElapsed bool

	// PreconditionsOK means the provider can bind the delete to the object
	// generation the scan observed and that generation is still current (R5).
	// Providers without generation preconditions are never eligible.
	PreconditionsOK bool
}

// DecideReclaim returns the verdict and the reason that belongs in the audit
// trail. Every refusal names the input that was missing or the condition that
// held, so an operator can tell a policy decision from a bug.
func DecideReclaim(in ReclaimInput) ReclaimDecision {
	switch {
	case in.ManifestErr != nil:
		return ReclaimDecision{Reason: fmt.Sprintf("manifest unreadable or untrusted: %v", in.ManifestErr)}
	case !in.ReferenceKnown:
		return ReclaimDecision{Reason: "reference graph unavailable; a descendant may still need this set"}
	case in.Referenced:
		return ReclaimDecision{Reason: "still referenced by a live descendant"}
	case !in.PublishedBeforeScan:
		return ReclaimDecision{Reason: "published inside the scan window"}
	case !in.Tombstoned:
		return ReclaimDecision{Reason: "no tombstone; deletion was never authorised"}
	case !in.GraceElapsed:
		return ReclaimDecision{Reason: "inside the grace window"}
	case !in.PreconditionsOK:
		return ReclaimDecision{Reason: "provider cannot fence the delete on the observed generation"}
	}

	return ReclaimDecision{Candidate: true, Reason: "unreferenced, tombstoned and past grace; generation still matches"}
}
