package storage

import (
	"bytes"
	"context"
	"fmt"
)

// maxArtifactManifestBytes caps what the reader will buffer: a manifest is a
// small record by design, so an oversized object at its path is corrupt (or
// something else entirely) and is refused rather than read whole.
const maxArtifactManifestBytes = 1 << 20

// WriteArtifactManifest stores a set's manifest. It must be the last write of a
// publish: the record claims every object it lists is already durable, and a
// manifest that arrives before its objects would authorise a deletion of a set
// that is still being written (S-03's commit order, extended to the manifest).
//
// The write goes through the provider's own put path — the same atomic
// temp-file-and-rename the data objects use — so a torn record cannot appear:
// a reader finds either the whole manifest or none at all.
func WriteArtifactManifest(ctx context.Context, provider StorageProvider, p Paths, m ArtifactManifest, opts ...PutOption) error {
	raw, err := MarshalArtifactManifest(m)
	if err != nil {
		return err
	}

	blob, err := provider.OpenBlob(ctx, ArtifactManifestPath(p))
	if err != nil {
		return fmt.Errorf("open manifest object for %s: %w", p.BuildID, err)
	}

	if err := blob.Put(ctx, raw, opts...); err != nil {
		return fmt.Errorf("write manifest for %s: %w", p.BuildID, err)
	}

	return nil
}

// ReadArtifactManifest reads and parses a set's manifest. A set without one is
// reported through the provider's own not-exist error (see ErrObjectNotExist):
// the reconciler treats absence like every other unproven input, so a set
// nobody described is never a candidate — and never deleted.
func ReadArtifactManifest(ctx context.Context, provider StorageProvider, p Paths) (ArtifactManifest, error) {
	blob, err := provider.OpenBlob(ctx, ArtifactManifestPath(p))
	if err != nil {
		return ArtifactManifest{}, err
	}

	limited := &manifestLimitWriter{limit: maxArtifactManifestBytes}
	if _, err := blob.WriteTo(ctx, limited); err != nil {
		return ArtifactManifest{}, fmt.Errorf("read manifest for %s: %w", p.BuildID, err)
	}

	return ParseArtifactManifest(limited.buf.Bytes())
}

// manifestLimitWriter refuses a copy once it would exceed the cap, so a corrupt
// or foreign object at the manifest path cannot be buffered whole.
type manifestLimitWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *manifestLimitWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		return 0, fmt.Errorf("%w: manifest exceeds %d bytes", ErrManifestMalformed, w.limit)
	}

	return w.buf.Write(p)
}
