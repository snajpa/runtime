package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const testBlockSize = 4096

// blockAligned builds a payload the artifact format accepts: header mappings are
// block-granular, so a payload has to be a whole number of blocks.
func blockAligned(blocks int, fill byte) []byte {
	data := make([]byte, blocks*testBlockSize)
	for i := range data {
		data[i] = fill
	}

	return data
}

func testProvider(t *testing.T) (storage.StorageProvider, storage.Spec, string) {
	t.Helper()

	dir := t.TempDir()

	spec, err := storage.ParseStorageURL("file://" + dir)
	if err != nil {
		t.Fatalf("parse storage url: %v", err)
	}

	provider, err := storage.NewProvider(t.Context(), spec)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}

	return provider, spec, dir
}

func testCompressConfig(codec string) storage.CompressConfig {
	return storage.CompressConfig{
		Enabled:            true,
		Type:               codec,
		Level:              2,
		FrameSizeKB:        2048,
		MinPartSizeMB:      5,
		FrameEncodeWorkers: 2,
		EncoderConcurrency: 1,
	}
}

// writeArtifact lays down one artifact: a payload (framed or plain), then a
// header at the requested version. It returns the payload's storage key.
func writeArtifact(t *testing.T, provider storage.StorageProvider, buildID uuid.UUID, kind artifactKind, data []byte, headerVersion uint64, framed bool) string {
	t.Helper()

	if len(data)%testBlockSize != 0 {
		t.Fatalf("test payload must be block-aligned, got %d bytes", len(data))
	}

	paths := storage.Paths{BuildID: buildID.String()}

	local := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	bd := header.BuildData{Size: int64(len(data))}

	var payloadKey string

	if framed {
		payloadKey = paths.DataFile(kind.name, storage.CompressionZstd)

		seekable, err := provider.OpenSeekable(t.Context(), payloadKey)
		if err != nil {
			t.Fatalf("open seekable: %v", err)
		}

		fullFT, checksum, err := seekable.StoreFile(t.Context(), local, storage.WithCompressConfig(testCompressConfig(storage.CompressionZstd.String())))
		if err != nil {
			t.Fatalf("store payload: %v", err)
		}

		bd.Checksum = checksum
		bd.FrameData = fullFT.Table()
	} else {
		payloadKey = paths.DataFile(kind.name, storage.CompressionNone)

		blob, err := provider.OpenBlob(t.Context(), payloadKey)
		if err != nil {
			t.Fatalf("open blob: %v", err)
		}

		if err := blob.Put(t.Context(), data); err != nil {
			t.Fatalf("put payload: %v", err)
		}

		bd.Checksum = sha256.Sum256(data)
	}

	metadata := header.NewTemplateMetadata(buildID, testBlockSize, uint64(len(data)))

	spec, err := header.NewHeader(metadata, []header.BuildMap{{
		Offset:             0,
		Length:             uint64(len(data)),
		BuildId:            buildID,
		BuildStorageOffset: 0,
	}})
	if err != nil {
		t.Fatalf("new header: %v", err)
	}

	spec.SetBuild(buildID, bd)

	if _, _, _, err := header.StoreHeader(t.Context(), provider, paths.HeaderFile(kind.name), spec.CloneForUpload(headerVersion)); err != nil {
		t.Fatalf("store header: %v", err)
	}

	return payloadKey
}

func headerVersionOf(t *testing.T, provider storage.StorageProvider, buildID uuid.UUID, kind artifactKind) uint64 {
	t.Helper()

	loaded, _, err := header.LoadHeader(t.Context(), provider, storage.Paths{BuildID: buildID.String()}.HeaderFile(kind.name))
	if err != nil {
		t.Fatalf("load header: %v", err)
	}

	return loaded.Metadata.Version
}

func migrateOptions() options {
	return options{
		mode:                modeMigrate,
		dryRun:              false,
		verify:              true,
		concurrency:         1,
		targetHeaderVersion: header.MetadataVersionV5,
		compressLevel:       defaultLevel,
		frameSizeKB:         defaultFrameSizeKB,
	}
}

func TestMigrateHeaderOnlyRewrite(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	payload := writeArtifact(t, provider, buildID, kind, blockAligned(1, 0xAB), header.MetadataVersionV4, true)

	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, kind, migrateOptions(), storage.CompressConfig{}, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionMigrate {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionMigrate, outcome.Detail)
	}

	if got := headerVersionOf(t, provider, buildID, kind); got != header.MetadataVersionV5 {
		t.Fatalf("header version = %d, want 5", got)
	}

	if outcome.FromPath != payload || outcome.ToPath != payload {
		t.Fatalf("payload moved unexpectedly: %s -> %s", outcome.FromPath, outcome.ToPath)
	}

	if len(outcome.SupersededPaths) != 0 {
		t.Fatalf("header-only rewrite reported a superseded payload: %v", outcome.SupersededPaths)
	}

	if err := verifyArtifact(t.Context(), provider, buildID, kind, payload); err != nil {
		t.Fatalf("verify after migration: %v", err)
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	writeArtifact(t, provider, buildID, kind, blockAligned(1, 0x11), header.MetadataVersionV4, true)

	opts := migrateOptions()
	opts.dryRun = true

	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, kind, opts, storage.CompressConfig{}, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionPlanned {
		t.Fatalf("action = %s, want %s", outcome.Action, actionPlanned)
	}

	if got := headerVersionOf(t, provider, buildID, kind); got != header.MetadataVersionV4 {
		t.Fatalf("dry run changed the header version to %d", got)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	writeArtifact(t, provider, buildID, kind, blockAligned(1, 0x22), header.MetadataVersionV5, true)

	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, kind, migrateOptions(), storage.CompressConfig{}, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionSkip {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionSkip, outcome.Detail)
	}
}

func TestMigrateRefusesUnverifiablePayload(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	payloadKey := writeArtifact(t, provider, buildID, kind, blockAligned(1, 0x33), header.MetadataVersionV4, true)

	// Overwrite the payload with different, block-aligned bytes: the header now
	// disagrees with its payload, so the tool must refuse to migrate over it.
	tampered := filepath.Join(t.TempDir(), "tampered")
	if err := os.WriteFile(tampered, blockAligned(1, 0x44), 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	seekable, err := provider.OpenSeekable(t.Context(), payloadKey)
	if err != nil {
		t.Fatalf("open seekable: %v", err)
	}

	if _, _, err := seekable.StoreFile(t.Context(), tampered, storage.WithCompressConfig(testCompressConfig(storage.CompressionZstd.String()))); err != nil {
		t.Fatalf("store tampered: %v", err)
	}

	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, kind, migrateOptions(), storage.CompressConfig{}, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionFailed {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionFailed, outcome.Detail)
	}

	if got := headerVersionOf(t, provider, buildID, kind); got != header.MetadataVersionV4 {
		t.Fatalf("header was rewritten despite the failed verification (version %d)", got)
	}
}

func TestMigrateReencodeAndSupersededHandling(t *testing.T) {
	t.Parallel()

	provider, spec, dir := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	sourceKey := writeArtifact(t, provider, buildID, kind, blockAligned(2, 0x55), header.MetadataVersionV4, true)

	opts := migrateOptions()
	opts.compressType = storage.CompressionLZ4.String()
	opts.deleteSuperseded = true
	opts.confirm = false

	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, kind, opts, testCompressConfig(storage.CompressionLZ4.String()), true)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionMigrate {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionMigrate, outcome.Detail)
	}

	targetKey := storage.Paths{BuildID: buildID.String()}.DataFile(kind.name, storage.CompressionLZ4)
	if outcome.ToPath != targetKey {
		t.Fatalf("to_path = %s, want %s", outcome.ToPath, targetKey)
	}

	if len(outcome.SupersededPaths) != 1 || outcome.SupersededPaths[0] != sourceKey {
		t.Fatalf("superseded = %v, want [%s]", outcome.SupersededPaths, sourceKey)
	}

	// Without -confirm the superseded payload must still be on disk.
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sourceKey))); err != nil {
		t.Fatalf("superseded payload removed without -confirm: %v", err)
	}

	if err := verifyArtifact(t.Context(), provider, buildID, kind, targetKey); err != nil {
		t.Fatalf("verify re-encoded artifact: %v", err)
	}

	// With -confirm (and the backend helper) the superseded payload goes away,
	// while the artifact keeps verifying.
	store, err := storeFor(t.Context(), spec)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	opts.confirm = true

	second, err := migrateArtifact(t.Context(), provider, store, buildID, kind, opts, testCompressConfig(storage.CompressionLZ4.String()), true)
	if err != nil {
		t.Fatalf("migrate with confirm: %v", err)
	}

	if second.Action != actionCleanup {
		t.Fatalf("second run action = %s, want %s (%s)", second.Action, actionCleanup, second.Detail)
	}

	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sourceKey))); err == nil {
		t.Fatalf("superseded payload still present after -delete-superseded -confirm")
	}

	if err := verifyArtifact(t.Context(), provider, buildID, kind, targetKey); err != nil {
		t.Fatalf("verify after superseded deletion: %v", err)
	}
}

func TestMigrateRefusesFramelessSourceWithoutReencode(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	// A header without frame data (the V3 shape): V4/V5 need frames, so a
	// header-only migration cannot work and must say so instead of writing a
	// header its payload does not support.
	writeArtifact(t, provider, buildID, kind, blockAligned(1, 0x66), 3, false)

	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, kind, migrateOptions(), storage.CompressConfig{}, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionFailed {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionFailed, outcome.Detail)
	}

	if got := headerVersionOf(t, provider, buildID, kind); got != 3 {
		t.Fatalf("header version changed to %d despite the refusal", got)
	}
}

func TestRunMigrateFailsOnFailedArtifacts(t *testing.T) {
	t.Parallel()

	provider, _, dir := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	// The rootfs payload is tampered behind its header (a failed artifact); the
	// memfile is simply absent (a missing one). Only the failure may fail the run.
	payloadKey := writeArtifact(t, provider, buildID, kind, blockAligned(1, 0x5A), header.MetadataVersionV4, true)

	tampered := filepath.Join(t.TempDir(), "tampered")
	if err := os.WriteFile(tampered, blockAligned(1, 0x6B), 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	seekable, err := provider.OpenSeekable(t.Context(), payloadKey)
	if err != nil {
		t.Fatalf("open seekable: %v", err)
	}

	if _, _, err := seekable.StoreFile(t.Context(), tampered, storage.WithCompressConfig(testCompressConfig(storage.CompressionZstd.String()))); err != nil {
		t.Fatalf("store tampered: %v", err)
	}

	reportPath := filepath.Join(t.TempDir(), "report.json")

	opts := migrateOptions()
	opts.builds = buildList{buildID.String()}
	opts.storageURL = "file://" + dir
	opts.reportPath = reportPath

	err = runMigrate(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "1 artifact(s) failed") {
		t.Fatalf("runMigrate = %v, want the failed-artifact error", err)
	}

	report, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatalf("read report: %v", readErr)
	}

	if !strings.Contains(string(report), `"failed":1`) {
		t.Fatalf("report does not carry the failed outcome:\n%s", report)
	}
}

func TestRunMigrateExitsCleanWithoutFailures(t *testing.T) {
	t.Parallel()

	provider, _, dir := testProvider(t)
	buildID := uuid.New()
	kind := artifactKind{name: storage.RootfsName, label: "rootfs"}

	// A healthy artifact (migrated) plus an absent one (missing): nothing
	// failed, so the run must exit successfully.
	writeArtifact(t, provider, buildID, kind, blockAligned(1, 0x7C), header.MetadataVersionV4, true)

	opts := migrateOptions()
	opts.builds = buildList{buildID.String()}
	opts.storageURL = "file://" + dir

	if err := runMigrate(t.Context(), opts); err != nil {
		t.Fatalf("runMigrate = %v, want success", err)
	}
}

func TestCompatWindow(t *testing.T) {
	t.Parallel()

	provider, _, _ := testProvider(t)
	buildID := uuid.New()
	paths := storage.Paths{BuildID: buildID.String()}

	// Older write format: the current code must read it (read-before-write is
	// what makes a mixed fleet safe during a rollout).
	writeArtifact(t, provider, buildID, rootfsKind, blockAligned(1, 0xE1), header.MetadataVersionV4, true)

	loaded, _, err := header.LoadHeader(t.Context(), provider, paths.HeaderFile(rootfsKind.name))
	if err != nil {
		t.Fatalf("current code must read the older format: %v", err)
	}

	if loaded.Metadata.Version != header.MetadataVersionV4 {
		t.Fatalf("header version = %d, want V4", loaded.Metadata.Version)
	}

	// Migrate onto the current format, then read that too: both live in the same
	// tree while the fleet rolls forward.
	outcome, err := migrateArtifact(t.Context(), provider, nil, buildID, rootfsKind, migrateOptions(), storage.CompressConfig{}, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if outcome.Action != actionMigrate {
		t.Fatalf("action = %s, want %s (%s)", outcome.Action, actionMigrate, outcome.Detail)
	}

	if got := headerVersionOf(t, provider, buildID, rootfsKind); got != header.MetadataVersionV5 {
		t.Fatalf("header version = %d, want V5", got)
	}

	if err := verifyArtifact(t.Context(), provider, buildID, rootfsKind, outcome.ToPath); err != nil {
		t.Fatalf("migrated artifact must verify: %v", err)
	}

	// A format the code does not know is refused loudly on the write side rather
	// than guessed at.
	metadata := header.NewTemplateMetadata(buildID, testBlockSize, testBlockSize)

	future, err := header.NewHeader(metadata, []header.BuildMap{{
		Offset:             0,
		Length:             testBlockSize,
		BuildId:            buildID,
		BuildStorageOffset: 0,
	}})
	if err != nil {
		t.Fatalf("new header: %v", err)
	}

	if _, err := header.SerializeHeader(future.CloneForUpload(6)); err == nil {
		t.Fatal("an unsupported header version must be refused loudly")
	}
}

func TestRunMigrateRejectsOutOfRangeRate(t *testing.T) {
	t.Parallel()

	_, _, dir := testProvider(t)

	for _, rate := range []int{-1, maxRatePerSecond + 1} {
		opts := migrateOptions()
		opts.builds = buildList{uuid.New().String()}
		opts.storageURL = "file://" + dir
		opts.rate = rate

		err := runMigrate(t.Context(), opts)
		if err == nil || !strings.Contains(err.Error(), "-rate") {
			t.Fatalf("runMigrate(rate=%d) = %v, want the -rate range error", rate, err)
		}
	}

	// The bound itself stays valid: 1e9/s is a 1 ns ticker interval.
	opts := migrateOptions()
	opts.builds = buildList{uuid.New().String()}
	opts.storageURL = "file://" + dir
	opts.rate = maxRatePerSecond

	if err := runMigrate(t.Context(), opts); err != nil {
		t.Fatalf("runMigrate(rate=%d) = %v, want success at the bound", maxRatePerSecond, err)
	}
}
