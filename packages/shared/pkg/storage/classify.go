package storage

import (
	"context"
	"errors"
)

// ErrorClass is the shared classification of a storage failure (REQ-A5): what
// the failure means for retry, refresh and degradation policy, independent of
// the provider that produced it. Providers map their native errors onto the
// package sentinels (ErrObjectNotExist, ErrObjectRateLimited, …) and this
// classification maps those sentinels onto one table.
type ErrorClass int

const (
	// ClassUnknown is an unclassified failure. Callers must choose their own
	// policy; the conservative default is not to retry blindly.
	ClassUnknown ErrorClass = iota
	// ClassNotFound means the object is absent: refresh the reference or
	// recreate it; a blind retry of the same read cannot recover it.
	ClassNotFound
	// ClassTransient means a retry can succeed: rate limiting, contention,
	// peer transitions, per-attempt deadlines, IO and provider 5xx errors.
	ClassTransient

	// ClassCanceled means the caller's context ended: stop, do not retry.
	ClassCanceled
	// ClassIntegrity means stored bytes failed (or could not be) verified:
	// refetch and verify rather than serving them; a blind retry of the same
	// copy cannot heal it.
	ClassIntegrity
	// ClassPermanent means retrying cannot help: unsupported capability,
	// metadata or signed-URL gaps, and soft-deleted objects.
	ClassPermanent
)

// ClassifyError maps err (and any errors it wraps) onto the shared taxonomy.
// The mapping is the contract; providers use the sentinels named here.
func ClassifyError(err error) ErrorClass {
	switch {
	case err == nil:
		return ClassUnknown
	case errors.Is(err, context.Canceled):
		return ClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		// A per-attempt deadline: the next attempt gets a fresh budget.
		return ClassTransient
	case errors.Is(err, ErrObjectNotExist):
		return ClassNotFound
	case errors.Is(err, ErrObjectRateLimited):
		return ClassTransient
	case errors.Is(err, ErrDigestMismatch), errors.Is(err, ErrDigestUnknown):
		return ClassIntegrity
	case errors.Is(err, ErrCapabilityUnsupported),
		errors.Is(err, ErrObjectSoftDeleted),
		errors.Is(err, ErrSignedUploadURLUnsupported),
		errors.Is(err, ErrMetadataUnsupported):
		return ClassPermanent
	}

	if _, ok := errors.AsType[*PeerTransitionedError](err); ok {
		// A routing signal, not a failure: re-resolve and read again.
		return ClassTransient
	}

	return ClassUnknown
}

// IsRetryable reports whether a blind retry of the same operation can succeed.
// Only transient failures qualify; callers that deliberately retry everything
// (upload snapshots) keep their own policy and consult ClassifyError instead.
func IsRetryable(err error) bool {
	return ClassifyError(err) == ClassTransient
}
