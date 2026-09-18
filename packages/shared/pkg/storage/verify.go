package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ErrDigestMismatch reports that stored bytes did not match the digest recorded
// for them (REQ-A2). It is typed so callers can evict the corrupt copy and
// refetch instead of serving it.
var ErrDigestMismatch = errors.New("object digest mismatch")

// ErrDigestUnknown reports that no digest is recorded for the object, so its
// integrity cannot be verified. Callers choose whether that is acceptable for
// their policy; legacy objects carry no checksum.
var ErrDigestUnknown = errors.New("object digest unknown")

// DigestMismatchError carries the expected and actual digests of a failed
// verification. It unwraps to ErrDigestMismatch.
type DigestMismatchError struct {
	Path     string
	Expected [32]byte
	Actual   [32]byte
}

func (e *DigestMismatchError) Error() string {
	return fmt.Sprintf("object %s digest mismatch: expected %x, got %x", e.Path, e.Expected, e.Actual)
}

// Unwrap makes the typed error match ErrDigestMismatch.
func (e *DigestMismatchError) Unwrap() error { return ErrDigestMismatch }

// VerifySHA256 streams r through SHA-256 with a bounded buffer and compares the
// result with want. A zero want means no digest was recorded (ErrDigestUnknown).
func VerifySHA256(path string, r io.Reader, want [32]byte) error {
	if want == ([32]byte{}) {
		return fmt.Errorf("%s: %w", path, ErrDigestUnknown)
	}

	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}

	var got [32]byte
	copy(got[:], h.Sum(nil))

	return compareDigest(path, want, got)
}

// VerifyObject verifies a stored object against its recorded digest (REQ-A2):
// the scrub primitive for whole-object integrity. With a frame table each
// frame is read through the provider's ranged reader — which decodes it and,
// on Close, drains and CRC-verifies it — before its uncompressed bytes are
// hashed; without one the stored bytes are hashed directly. It never writes,
// evicts or refetches — the caller owns self-healing on ErrDigestMismatch.
func VerifyObject(ctx context.Context, store StorageProvider, path string, ft *FrameTable, want [32]byte) error {
	if want == ([32]byte{}) {
		return fmt.Errorf("%s: %w", path, ErrDigestUnknown)
	}

	if ft == nil || !ft.IsCompressed() {
		return verifyStoredBytes(ctx, store, path, want)
	}

	seekable, err := store.OpenSeekable(ctx, path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	// The provider's ranged reader decodes the frame containing the requested
	// uncompressed offset, and its Close drains and CRC-verifies that frame, so
	// a corrupt frame fails before its bytes are counted. Frames are hashed in
	// order to reproduce the object's uncompressed byte stream.
	h := sha256.New()

	for i := range ft.NumFrames() {
		startU, endU, _, _ := ft.FrameAt(i)

		reader, _, err := seekable.OpenRangeReader(ctx, startU, endU-startU, ft)
		if err != nil {
			return fmt.Errorf("read frame %d of %s: %w", i, path, err)
		}

		if _, err := io.Copy(h, reader); err != nil {
			_, _ = reader.Close(ctx)

			return fmt.Errorf("hash frame %d of %s: %w", i, path, err)
		}
		if _, err := reader.Close(ctx); err != nil {
			return fmt.Errorf("verify frame %d of %s: %w", i, path, err)
		}
	}

	var got [32]byte
	copy(got[:], h.Sum(nil))

	return compareDigest(path, want, got)
}

// verifyStoredBytes hashes an object whose stored bytes are its data
// (uncompressed objects).
func verifyStoredBytes(ctx context.Context, store StorageProvider, path string, want [32]byte) error {
	blob, err := store.OpenBlob(ctx, path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	h := sha256.New()
	if _, err := blob.WriteTo(ctx, h); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var got [32]byte
	copy(got[:], h.Sum(nil))

	return compareDigest(path, want, got)
}

// compareDigest is the shared digest decision: unknown, mismatch or verified.
func compareDigest(path string, want, got [32]byte) error {
	if want == ([32]byte{}) {
		return fmt.Errorf("%s: %w", path, ErrDigestUnknown)
	}
	if got != want {
		return &DigestMismatchError{Path: path, Expected: want, Actual: got}
	}

	return nil
}
