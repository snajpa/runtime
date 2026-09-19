package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

var (
	rootfsKind  = artifactKind{name: storage.RootfsName, label: "rootfs"}
	memfileKind = artifactKind{name: storage.MemfileName, label: "memfile"}
)

func reconcileOptions(verify bool) options {
	return options{
		mode:                modeReconcile,
		verify:              verify,
		dryRun:              true,
		targetHeaderVersion: header.MetadataVersionV5,
	}
}

func newTestReporter() *reporter {
	return &reporter{counts: map[string]int{}}
}

// writeBuild lays down both artifacts of a build, which is what reconcile is
// expected to see for a complete build.
func writeBuild(t *testing.T, provider storage.StorageProvider, buildID uuid.UUID) {
	t.Helper()

	writeArtifact(t, provider, buildID, rootfsKind, blockAligned(1, 0x77), header.MetadataVersionV5, true)
	writeArtifact(t, provider, buildID, memfileKind, blockAligned(1, 0x78), header.MetadataVersionV5, true)
}

func TestReconcileHealthyBuildIsComplete(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()

	writeBuild(t, provider, buildID)

	for _, kind := range artifactKinds() {
		outcome := reconcileArtifact(t.Context(), reconcileOptions(true), provider, buildID, kind)
		if outcome.Action != actionComplete {
			t.Fatalf("%s: action = %s, want %s (%s)", kind.label, outcome.Action, actionComplete, outcome.Detail)
		}
	}
}

func TestReconcilePresenceOnlyMode(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()

	writeBuild(t, provider, buildID)

	outcome := reconcileArtifact(t.Context(), reconcileOptions(false), provider, buildID, rootfsKind)
	if outcome.Action != actionComplete {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionComplete, outcome.Detail)
	}

	if outcome.Detail != " present (not verified)" {
		t.Fatalf("detail = %q, want presence-only note", outcome.Detail)
	}
}

func TestReconcileReportsMissingPayload(t *testing.T) {
	t.Parallel()

	provider, _, dir := testProvider(t)
	buildID := uuid.New()

	writeBuild(t, provider, buildID)

	payloadKey := storage.Paths{BuildID: buildID.String()}.DataFile(rootfsKind.name, storage.CompressionZstd)

	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(payloadKey))); err != nil {
		t.Fatalf("remove payload: %v", err)
	}

	outcome := reconcileArtifact(t.Context(), reconcileOptions(true), provider, buildID, rootfsKind)
	if outcome.Action != actionMissingPayload {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionMissingPayload, outcome.Detail)
	}

	// The other artifact of the same build is untouched by that.
	other := reconcileArtifact(t.Context(), reconcileOptions(true), provider, buildID, memfileKind)
	if other.Action != actionComplete {
		t.Fatalf("memfile action = %s, want %s (%s)", other.Action, actionComplete, other.Detail)
	}
}

func TestReconcileReportsMismatch(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()

	writeBuild(t, provider, buildID)

	payloadKey := storage.Paths{BuildID: buildID.String()}.DataFile(rootfsKind.name, storage.CompressionZstd)

	tampered := filepath.Join(t.TempDir(), "tampered")
	if err := os.WriteFile(tampered, blockAligned(1, 0xAA), 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	seekable, err := provider.OpenSeekable(t.Context(), payloadKey)
	if err != nil {
		t.Fatalf("open seekable: %v", err)
	}

	if _, _, err := seekable.StoreFile(t.Context(), tampered, storage.WithCompressConfig(testCompressConfig(storage.CompressionZstd.String()))); err != nil {
		t.Fatalf("store tampered: %v", err)
	}

	outcome := reconcileArtifact(t.Context(), reconcileOptions(true), provider, buildID, rootfsKind)
	if outcome.Action != actionMismatch {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionMismatch, outcome.Detail)
	}
}

func TestReconcileScanPrefixFindsSupersededPayload(t *testing.T) {
	t.Parallel()

	provider, spec, _ := testProvider(t)
	buildID := uuid.New()

	writeBuild(t, provider, buildID)

	// A leftover codec variant next to the live payload is what a migration
	// leaves behind: the scan must report it, and must not delete it without the
	// explicit flags.
	stale := storage.Paths{BuildID: buildID.String()}.DataFile(rootfsKind.name, storage.CompressionLZ4)

	seekable, err := provider.OpenSeekable(t.Context(), stale)
	if err != nil {
		t.Fatalf("open seekable: %v", err)
	}

	local := filepath.Join(t.TempDir(), "stale")
	if err := os.WriteFile(local, blockAligned(1, 0xCC), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if _, _, err := seekable.StoreFile(t.Context(), local, storage.WithCompressConfig(testCompressConfig(storage.CompressionLZ4.String()))); err != nil {
		t.Fatalf("store stale: %v", err)
	}

	rep := newTestReporter()

	opts := reconcileOptions(true)
	opts.scanPrefix = buildID.String()

	if err := scanPrefix(t.Context(), opts, spec, provider, []string{buildID.String()}, rep); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if rep.counts[actionOrphan] != 1 {
		t.Fatalf("orphan = %d, want 1 (%v)", rep.counts[actionOrphan], rep.counts)
	}
}
