package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The classification table is the contract (REQ-A5): every sentinel maps to one
// class, wrapped errors classify like their cause, and unknown errors stay
// unknown instead of being treated as retryable.
func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{name: "nil", err: nil, want: ClassUnknown},
		{name: "plain error", err: errors.New("boom"), want: ClassUnknown},
		{name: "context canceled", err: context.Canceled, want: ClassCanceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: ClassTransient},
		{name: "object not exist", err: ErrObjectNotExist, want: ClassNotFound},
		{name: "rate limited", err: ErrObjectRateLimited, want: ClassTransient},
		{name: "digest mismatch", err: &DigestMismatchError{Path: "p"}, want: ClassIntegrity},
		{name: "digest unknown", err: ErrDigestUnknown, want: ClassIntegrity},
		{name: "capability unsupported", err: ErrCapabilityUnsupported, want: ClassPermanent},
		{name: "soft deleted", err: ErrObjectSoftDeleted, want: ClassPermanent},
		{name: "signed url unsupported", err: ErrSignedUploadURLUnsupported, want: ClassPermanent},
		{name: "metadata unsupported", err: ErrMetadataUnsupported, want: ClassPermanent},
		{name: "peer transitioned", err: &PeerTransitionedError{}, want: ClassTransient},
		{name: "wrapped not exist", err: fmt.Errorf("open x: %w", ErrObjectNotExist), want: ClassNotFound},
		{name: "wrapped rate limit", err: fmt.Errorf("put y: %w", ErrObjectRateLimited), want: ClassTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, ClassifyError(tt.err))
		})
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	require.True(t, IsRetryable(ErrObjectRateLimited))
	require.True(t, IsRetryable(context.DeadlineExceeded))
	require.True(t, IsRetryable(&PeerTransitionedError{}))

	require.False(t, IsRetryable(nil))
	require.False(t, IsRetryable(ErrObjectNotExist), "a missing object needs a refresh, not a blind retry")
	require.False(t, IsRetryable(context.Canceled))
	require.False(t, IsRetryable(ErrDigestMismatch), "a corrupt copy needs a refetch")
	require.False(t, IsRetryable(ErrCapabilityUnsupported))
	require.False(t, IsRetryable(errors.New("boom")), "unknown failures are not retried blindly")
}
