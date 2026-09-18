package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceStringsPopulated(t *testing.T) {
	t.Parallel()

	for s := range numSources {
		require.NotEmptyf(t, s.String(), "source %d has empty string label", s)
	}
}

func TestSeekableObjectTypeStrings(t *testing.T) {
	t.Parallel()

	require.Equal(t, "memfile", MemfileObjectType.String())
	require.Equal(t, "rootfs", RootFSObjectType.String())
	require.Equal(t, "unknown", UnknownSeekableObjectType.String())
	for o := range numSeekableObjectTypes {
		require.NotEmptyf(t, o.String(), "object type %d has empty file_type label", o)
	}
}

func TestOutcomeMapping(t *testing.T) {
	t.Parallel()

	require.Equal(t, OutcomeOK, Outcome(nil))
	require.Equal(t, OutcomeErrCanceled, Outcome(context.Canceled))
	require.Equal(t, OutcomeErrTimeout, Outcome(context.DeadlineExceeded))
	require.Equal(t, OutcomeErrIO, Outcome(errors.New("boom")))
}

// TestPrecomputedAttrsPopulated guards the invariant that every emission site
// finds a non-nil precomputed attribute set — no enum combination is missed by
// the init() loops.
func TestPrecomputedAttrsPopulated(t *testing.T) {
	t.Parallel()

	for o := range numSeekableObjectTypes {
		for s := range numSources {
			for c := range CompressionType(numCodecs) {
				require.NotNil(t, OKAttrs(o, s, c))
			}
		}
	}
}

// The residue recorder is the monitoring hook for uploads that end without a
// commit or a completed abort (REQ-A3); it must be safe on a plain context.
func TestRecordUploadResidue(t *testing.T) {
	t.Parallel()

	require.Equal(t, "provider", AttrProvider)
	require.NotPanics(t, func() { RecordUploadResidue(t.Context(), "azure") })
}
