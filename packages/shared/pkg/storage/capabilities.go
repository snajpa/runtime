package storage

import (
	"errors"
	"fmt"
)

// Capabilities is the per-backend capability matrix of the storage contract
// (REQ-B1, REQ-B5): the facts a caller may rely on without probing the
// backend. Providers publish it through CapabilityReporter; CapabilitiesOf
// returns the conservative default for providers that do not report.
type Capabilities struct {
	// Name is the short backend name used in diagnostics ("fs", "s3", "gcs",
	// "azure").
	Name string

	// DeleteBatchSize is the largest number of objects one delete request
	// carries. 0 means the backend deletes a whole prefix in a single native
	// operation (a local filesystem subtree).
	DeleteBatchSize int

	// AbortUpload reports whether an in-flight upload can be aborted and its
	// staged parts released. Azure block uploads cannot be aborted; the
	// service garbage-collects staged blocks.
	AbortUpload bool

	// SignedUploadURL reports whether UploadSignedURL returns a usable target
	// for this configuration. On the filesystem provider this is true only
	// when the local upload endpoint and its HMAC key are configured.
	SignedUploadURL bool

	// CustomMetadata reports whether object custom metadata round-trips
	// through the shared layer (BlobCustomMetadata / MetadataReader).
	CustomMetadata bool

	// Multipart reports whether large objects are uploaded in multiple parts
	// through a provider upload session.
	Multipart bool

	// MultipartMinPartSize is the smallest non-final part a multipart upload
	// accepts (S3 and the GCS XML API: 5 MiB). 0 means the provider imposes no
	// minimum, or Multipart is false.
	MultipartMinPartSize int64
}

// CapabilityReporter is implemented by providers that publish their
// capability matrix.
type CapabilityReporter interface {
	Capabilities() Capabilities
}

// DefaultCapabilities is the conservative fallback used when a provider does
// not report capabilities: single-object deletes, no abort, no signed URLs,
// no custom metadata, no multipart. Callers must never assume a capability a
// provider did not report.
var DefaultCapabilities = Capabilities{Name: "unknown", DeleteBatchSize: 1}

// CapabilitiesOf returns the capability matrix of p, falling back to
// DefaultCapabilities for nil or non-reporting providers.
func CapabilitiesOf(p StorageProvider) Capabilities {
	if p == nil {
		return DefaultCapabilities
	}

	reporter, ok := p.(CapabilityReporter)
	if !ok {
		return DefaultCapabilities
	}

	return reporter.Capabilities()
}

// ErrCapabilityUnsupported reports an explicit provider capability gap
// (REQ-B5): the backend cannot perform the requested operation. It is a
// permanent condition, distinct from transient provider failures.
var ErrCapabilityUnsupported = errors.New("storage provider capability not supported")

// CapabilityError is the typed capability-gap error. It unwraps to
// ErrCapabilityUnsupported, so errors.Is(err, ErrCapabilityUnsupported) holds
// for callers that do not need the provider/operation detail.
type CapabilityError struct {
	Provider   string
	Capability string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("storage provider %s does not support %s", e.Provider, e.Capability)
}

// Unwrap makes the typed error match ErrCapabilityUnsupported.
func (e *CapabilityError) Unwrap() error { return ErrCapabilityUnsupported }

// RequireAbortUpload returns a typed capability error when an in-flight
// upload cannot be aborted.
func (c Capabilities) RequireAbortUpload() error {
	if c.AbortUpload {
		return nil
	}

	return &CapabilityError{Provider: c.Name, Capability: "upload abort"}
}

// RequireSignedUploadURL returns a typed capability error when
// UploadSignedURL cannot produce a usable target.
func (c Capabilities) RequireSignedUploadURL() error {
	if c.SignedUploadURL {
		return nil
	}

	return &CapabilityError{Provider: c.Name, Capability: "signed upload URLs"}
}

// RequireCustomMetadata returns a typed capability error when object custom
// metadata does not round-trip.
func (c Capabilities) RequireCustomMetadata() error {
	if c.CustomMetadata {
		return nil
	}

	return &CapabilityError{Provider: c.Name, Capability: "custom metadata"}
}
