package storage

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func eligibleReclaim() ReclaimInput {
	return ReclaimInput{
		ReferenceKnown:      true,
		PublishedBeforeScan: true,
		Tombstoned:          true,
		GraceElapsed:        true,
		PreconditionsOK:     true,
	}
}

// The fully-proven case is the only one that may delete; every other input
// keeps the set, and the reason says which rule refused it (test-plan cases
// 9-11).
func TestDecideReclaimRefusesEverythingItCannotProve(t *testing.T) {
	t.Parallel()

	decision := DecideReclaim(eligibleReclaim())
	require.True(t, decision.Candidate)
	require.NotEmpty(t, decision.Reason)

	refusals := map[string]struct {
		breakIt    func(*ReclaimInput)
		wantReason string
	}{
		"unknown manifest version":    {func(i *ReclaimInput) { i.ManifestErr = ErrManifestUnknownVersion }, "manifest unreadable"},
		"unreadable reference graph":  {func(i *ReclaimInput) { i.ReferenceKnown = false }, "reference graph unavailable"},
		"still referenced":            {func(i *ReclaimInput) { i.Referenced = true }, "still referenced"},
		"published inside the scan":   {func(i *ReclaimInput) { i.PublishedBeforeScan = false }, "published inside the scan"},
		"no tombstone":                {func(i *ReclaimInput) { i.Tombstoned = false }, "no tombstone"},
		"inside the grace window":     {func(i *ReclaimInput) { i.GraceElapsed = false }, "inside the grace window"},
		"no generation preconditions": {func(i *ReclaimInput) { i.PreconditionsOK = false }, "cannot fence the delete"},
	}

	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			in := eligibleReclaim()
			tc.breakIt(&in)

			decision := DecideReclaim(in)
			require.False(t, decision.Candidate, "an unproven input must never be a candidate")
			require.Contains(t, decision.Reason, tc.wantReason, "the audit trail names the rule that refused")
		})
	}
}

// An absent manifest reaches the decision as the reader's not-exist error, and
// that must refuse: nil in ManifestErr means "a manifest was read and parsed",
// which absence is not (lane A's note on the reader/decision boundary).
func TestDecideReclaimRefusesAnAbsentManifest(t *testing.T) {
	t.Parallel()

	provider := newFileSystemStorage(t.TempDir(), "", nil)

	_, err := ReadArtifactManifest(t.Context(), provider, Paths{BuildID: "build-1"})
	require.ErrorIs(t, err, ErrObjectNotExist)

	in := eligibleReclaim()
	in.ManifestErr = err

	decision := DecideReclaim(in)
	require.False(t, decision.Candidate)
	require.Contains(t, decision.Reason, "manifest")
}

// Any manifest error is skip + alert, including the unknown-version case a
// naive reader could mistake for "no manifest" (lane E's P2), and including
// transient provider failures.
func TestDecideReclaimTreatsManifestErrorsAsRefusals(t *testing.T) {
	t.Parallel()

	manifestErrors := []error{
		ErrManifestUnknownVersion,
		ErrManifestUnknownClass,
		ErrManifestMalformed,
		errors.New("provider timeout"),
	}

	for _, err := range manifestErrors {
		in := eligibleReclaim()
		in.ManifestErr = err

		decision := DecideReclaim(in)
		require.False(t, decision.Candidate)
		require.Contains(t, decision.Reason, "manifest")
	}
}
