package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// artifactKind is one payload plus header pair a build owns.
type artifactKind struct {
	name  string
	label string
}

func artifactKinds() []artifactKind {
	return []artifactKind{
		{name: storage.MemfileName, label: "memfile"},
		{name: storage.RootfsName, label: "rootfs"},
	}
}

// Outcome actions, as recorded in the report. "planned" is what a dry run
// reports for work it would do; "migrate" means the artifact was rewritten and
// re-verified.
const (
	actionSkip    = "skip"
	actionMigrate = "migrate"
	actionPlanned = "planned"
	actionMissing = "missing"
	actionFailed  = "failed"
	// actionCleanup means the artifact was already on the target format and the
	// run only removed superseded leftovers (explicitly requested).
	actionCleanup = "cleanup"
)

type artifactOutcome struct {
	Build             string   `json:"build"`
	Artifact          string   `json:"artifact"`
	Action            string   `json:"action"`
	FromPath          string   `json:"from_path,omitempty"`
	ToPath            string   `json:"to_path,omitempty"`
	FromHeaderVersion uint64   `json:"from_header_version,omitempty"`
	ToHeaderVersion   uint64   `json:"to_header_version,omitempty"`
	Bytes             int64    `json:"bytes,omitempty"`
	SupersededPaths   []string `json:"superseded_paths,omitempty"`
	DurationMS        int64    `json:"duration_ms"`
	Detail            string   `json:"detail,omitempty"`
}

// payloadCandidates lists the paths a payload can live at, most specific first.
// The storage abstraction has no list operation, so migration probes; the
// candidates come from the product's own path rules (compression suffix).
func payloadCandidates(paths storage.Paths, name string) []string {
	return []string{
		paths.DataFile(name, storage.CompressionZstd),
		paths.DataFile(name, storage.CompressionLZ4),
		paths.DataFile(name, storage.CompressionNone),
	}
}

type bytesBuffer struct{ data []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)

	return len(p), nil
}

func (b *bytesBuffer) Bytes() []byte { return b.data }

var _ io.Writer = (*bytesBuffer)(nil)

// readPayload reads a whole logical payload the way a node does: chunk-aligned
// range readers whose Close verifies every frame's CRC. A payload that cannot
// be read this way is never migrated.
func readPayload(ctx context.Context, provider storage.StorageProvider, path string, size int64, ft *storage.FrameTable) ([]byte, error) {
	seekable, err := provider.OpenSeekable(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	out := make([]byte, 0, size)

	for read := int64(0); read < size; {
		step := min(int64(storage.MemoryChunkSize), size-read)

		reader, _, err := seekable.OpenRangeReader(ctx, read, step, ft)
		if err != nil {
			return nil, fmt.Errorf("range at %d: %w", read, err)
		}

		var chunk bytesBuffer

		_, copyErr := io.Copy(&chunk, reader)

		_, closeErr := reader.Close(ctx)

		switch {
		case copyErr != nil:
			return nil, fmt.Errorf("read at %d: %w", read, copyErr)
		case closeErr != nil:
			return nil, fmt.Errorf("frame verification at %d: %w", read, closeErr)
		case len(chunk.Bytes()) == 0:
			return nil, fmt.Errorf("read at %d made no progress", read)
		}

		out = append(out, chunk.Bytes()...)
		read += int64(len(chunk.Bytes()))
	}

	return out, nil
}

// targetCompressConfig resolves the payload encoding a migration writes with.
// An empty -compress-type keeps the source payload, which makes the migration a
// header-only rewrite - the cheapest correct move. A codec asks for a full
// payload rewrite; "none" is refused because V4/V5 headers carry frame tables
// and an uncompressed store produces none.
func targetCompressConfig(opts options) (storage.CompressConfig, bool, error) {
	switch opts.compressType {
	case "":
		return storage.CompressConfig{}, false, nil
	case "zstd", "lz4":
		cfg := storage.CompressConfig{
			Enabled:            true,
			Type:               opts.compressType,
			Level:              opts.compressLevel,
			FrameSizeKB:        opts.frameSizeKB,
			MinPartSizeMB:      5,
			FrameEncodeWorkers: 4,
			EncoderConcurrency: 1,
		}
		if err := cfg.Validate(); err != nil {
			return storage.CompressConfig{}, false, fmt.Errorf("-compress-type %s: %w", opts.compressType, err)
		}

		return cfg, true, nil
	case "none":
		return storage.CompressConfig{}, false, errors.New("-compress-type none is not supported: V4/V5 headers carry frame tables, which an uncompressed store cannot produce")
	default:
		return storage.CompressConfig{}, false, fmt.Errorf("unknown -compress-type %q (want zstd or lz4)", opts.compressType)
	}
}

// objectExists reports whether one specific key is present.
func objectExists(ctx context.Context, provider storage.StorageProvider, key string) (bool, error) {
	blob, err := provider.OpenBlob(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("open %s: %w", key, err)
	}

	exists, err := blob.Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("exists %s: %w", key, err)
	}

	return exists, nil
}

// firstExistingCandidate returns the first candidate path that exists, without
// reading it. Reconcile uses it in presence-only mode, where a cheap answer is
// what is wanted; the verifying path uses resolvePayload instead.
func firstExistingCandidate(ctx context.Context, provider storage.StorageProvider, paths storage.Paths, name string) (string, error) {
	for _, candidate := range payloadCandidates(paths, name) {
		exists, err := objectExists(ctx, provider, candidate)
		if err != nil {
			return "", err
		}

		if exists {
			return candidate, nil
		}
	}

	return "", nil
}

// payloadResolution is what the tool learned about one artifact's payload.
type payloadResolution struct {
	// Path is the candidate that exists and verifies; empty when none does.
	Path string
	// Data is the verified payload.
	Data []byte
	// Superseded are candidates that exist but do not belong to the header any
	// more - the leftovers a re-encode leaves behind. They are reported, and
	// removed only when explicitly asked for.
	Superseded []string
}

// resolvePayload finds the payload that belongs to the header. The storage
// abstraction cannot list, and after a migration more than one candidate can
// exist (the superseded codec plus the new one), so every candidate is read and
// checked against the header: the one that verifies is the artifact, the others
// are leftovers. A candidate that exists but cannot be read is reported, never
// silently ignored.
func resolvePayload(ctx context.Context, provider storage.StorageProvider, paths storage.Paths, name string, bd header.BuildData, ft *storage.FrameTable) (payloadResolution, error) {
	var (
		res     payloadResolution
		lastErr error
	)

	for _, candidate := range payloadCandidates(paths, name) {
		exists, err := objectExists(ctx, provider, candidate)
		if err != nil {
			return res, err
		}

		if !exists {
			continue
		}

		data, err := readPayload(ctx, provider, candidate, bd.Size, ft)
		if err != nil {
			res.Superseded = append(res.Superseded, candidate)
			lastErr = fmt.Errorf("read %s: %w", candidate, err)

			continue
		}

		if sum := sha256.Sum256(data); sum != bd.Checksum {
			res.Superseded = append(res.Superseded, candidate)
			lastErr = fmt.Errorf("%s does not match the header checksum", candidate)

			continue
		}

		res.Path = candidate
		res.Data = data

		return res, nil
	}

	if len(res.Superseded) > 0 {
		// Candidates exist but none is the artifact the header describes.
		return res, lastErr
	}

	return res, nil
}

// verifyArtifact reads the artifact through the ordinary read path and checks it
// against the header it must agree with.
func verifyArtifact(ctx context.Context, provider storage.StorageProvider, buildID uuid.UUID, kind artifactKind, payloadPath string) error {
	paths := storage.Paths{BuildID: buildID.String()}

	loaded, _, err := header.LoadHeader(ctx, provider, paths.HeaderFile(kind.name))
	if err != nil {
		return fmt.Errorf("reload header: %w", err)
	}

	bd, ok := loaded.Builds[buildID]
	if !ok {
		return errors.New("header no longer describes this build")
	}

	data, err := readPayload(ctx, provider, payloadPath, bd.Size, loaded.GetBuildFrameData(buildID))
	if err != nil {
		return err
	}

	if sum := sha256.Sum256(data); sum != bd.Checksum {
		return errors.New("checksum mismatch after rewrite")
	}

	return nil
}

func migrateArtifact(ctx context.Context, provider storage.StorageProvider, store objectStore, buildID uuid.UUID, kind artifactKind, opts options, cfg storage.CompressConfig, reencode bool) (*artifactOutcome, error) {
	started := time.Now()
	paths := storage.Paths{BuildID: buildID.String()}
	headerPath := paths.HeaderFile(kind.name)

	outcome := &artifactOutcome{
		Build:             buildID.String(),
		Artifact:          kind.label,
		ToHeaderVersion:   opts.targetHeaderVersion,
		FromHeaderVersion: 0,
	}

	loaded, _, err := header.LoadHeader(ctx, provider, headerPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			outcome.Action = actionMissing
			outcome.Detail = " header not present"

			return outcome, nil
		}

		return outcome, fmt.Errorf("load header: %w", err)
	}

	outcome.FromHeaderVersion = loaded.Metadata.Version

	bd, ok := loaded.Builds[buildID]
	if !ok {
		outcome.Action = actionMissing
		outcome.Detail = " header does not describe this build"

		return outcome, nil
	}

	outcome.Bytes = bd.Size

	resolution, err := resolvePayload(ctx, provider, paths, kind.name, bd, loaded.GetBuildFrameData(buildID))
	if err != nil {
		// Never rewrite what does not verify first: a header that disagrees with
		// every payload it can see is a finding, not something to migrate over.
		outcome.Action = actionFailed
		outcome.Detail = " " + err.Error()

		//nolint:nilerr // the artifact's failure is the outcome; the tool's error
		// channel is reserved for run-level failures that must stop it.
		return outcome, nil
	}

	outcome.SupersededPaths = resolution.Superseded

	if resolution.Path == "" {
		outcome.Action = actionMissing
		outcome.Detail = " payload not present"

		return outcome, nil
	}

	srcPath := resolution.Path
	data := resolution.Data

	outcome.FromPath = srcPath
	outcome.ToPath = srcPath

	framesMissing := loaded.GetBuildFrameData(buildID) == nil
	headerCurrent := loaded.Metadata.Version == opts.targetHeaderVersion

	if reencode {
		outcome.ToPath = paths.DataFile(kind.name, cfg.CompressionType())
	}

	needPayloadWrite := reencode && srcPath != outcome.ToPath

	if reencode && srcPath == outcome.ToPath {
		// The payload is already on the requested codec; only the header may
		// still need the version bump, and leftovers may need reporting.
		needPayloadWrite = false
	}

	needHeaderWrite := !headerCurrent || needPayloadWrite

	// A V4/V5 target needs frame data; re-encoding is the only way to produce it
	// when the source has none.
	if framesMissing && !reencode {
		outcome.Action = actionFailed
		outcome.Detail = " source header carries no frame data; re-run with -compress-type zstd (or lz4) to re-encode the payload"

		return outcome, nil
	}

	detail := fmt.Sprintf(" header v%d -> v%d", loaded.Metadata.Version, opts.targetHeaderVersion)
	if needPayloadWrite {
		detail += fmt.Sprintf(", payload %s -> %s (%s)", srcPath, outcome.ToPath, cfg.CompressionType())
	} else {
		detail += ", payload untouched"
	}

	leftovers := make([]string, 0, len(outcome.SupersededPaths))
	for _, candidate := range outcome.SupersededPaths {
		if candidate != outcome.ToPath {
			leftovers = append(leftovers, candidate)
		}
	}

	workRemains := needHeaderWrite || len(leftovers) > 0
	if !workRemains {
		outcome.Action = actionSkip
		outcome.Detail = " already on the target format"

		return outcome, nil
	}

	if opts.dryRun {
		for _, candidate := range leftovers {
			switch {
			case !opts.deleteSuperseded:
				detail += fmt.Sprintf("; superseded %s kept", candidate)
			case !opts.confirm:
				detail += fmt.Sprintf("; superseded %s kept (needs -confirm)", candidate)
			default:
				detail += fmt.Sprintf("; superseded %s would be deleted", candidate)
			}
		}

		outcome.Action = actionPlanned
		outcome.Detail = detail
		outcome.DurationMS = time.Since(started).Milliseconds()

		return outcome, nil
	}

	if needPayloadWrite {
		if err := rewritePayload(ctx, provider, buildID, outcome.ToPath, data, cfg, opts, headerPath, loaded.Metadata.BlockSize); err != nil {
			return outcome, err
		}

		// The header written below describes the new payload; the old one becomes
		// a leftover and is handled with the others, never deleted implicitly.
		if srcPath != outcome.ToPath {
			leftovers = append(leftovers, srcPath)
		}
	} else if err := storeHeaderVersion(ctx, provider, headerPath, loaded, opts.targetHeaderVersion); err != nil {
		return outcome, err
	}

	if opts.verify {
		if err := verifyArtifact(ctx, provider, buildID, kind, outcome.ToPath); err != nil {
			outcome.Action = actionFailed
			outcome.Detail = " verify after rewrite: " + err.Error()

			//nolint:nilerr // reported as the artifact's outcome; the rewrite is
			// already done and the report must say so.
			return outcome, nil
		}
	}

	kept := make([]string, 0, len(leftovers))
	deleted := false

	for _, candidate := range leftovers {
		switch {
		case !opts.deleteSuperseded:
			kept = append(kept, candidate)
			detail += fmt.Sprintf("; superseded %s kept (pass -delete-superseded -confirm to remove)", candidate)
		case !opts.confirm:
			kept = append(kept, candidate)
			detail += fmt.Sprintf("; superseded %s kept (-delete-superseded needs -confirm)", candidate)
		case store == nil:
			return outcome, errors.New("deletion requested but this storage provider cannot delete single keys")
		default:
			if err := store.Delete(ctx, candidate); err != nil {
				return outcome, fmt.Errorf("delete superseded %s: %w", candidate, err)
			}

			detail += fmt.Sprintf("; superseded %s deleted", candidate)
			deleted = true
		}
	}

	outcome.SupersededPaths = kept

	switch {
	case needHeaderWrite || needPayloadWrite:
		outcome.Action = actionMigrate
	case deleted:
		outcome.Action = actionCleanup
	default:
		outcome.Action = actionSkip
	}

	outcome.Detail = detail
	outcome.DurationMS = time.Since(started).Milliseconds()

	return outcome, nil
}

// storeHeaderVersion rewrites an existing header at another format version,
// keeping its mapping and build data intact.
func storeHeaderVersion(ctx context.Context, provider storage.StorageProvider, headerPath string, loaded *header.Header, version uint64) error {
	if _, _, _, err := header.StoreHeader(ctx, provider, headerPath, loaded.CloneForUpload(version)); err != nil {
		return fmt.Errorf("store header: %w", err)
	}

	return nil
}

// rewritePayload stores the payload under the target path with the requested
// codec and writes a header that describes it.
func rewritePayload(ctx context.Context, provider storage.StorageProvider, buildID uuid.UUID, targetPath string, data []byte, cfg storage.CompressConfig, opts options, headerPath string, blockSize uint64) error {
	local, err := os.CreateTemp("", "migrate-builds-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}

	defer os.Remove(local.Name())

	if _, err := local.Write(data); err != nil {
		_ = local.Close()

		return fmt.Errorf("write temp file: %w", err)
	}

	if err := local.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	seekable, err := provider.OpenSeekable(ctx, targetPath)
	if err != nil {
		return fmt.Errorf("open target %s: %w", targetPath, err)
	}

	fullFT, checksum, err := seekable.StoreFile(ctx, local.Name(), storage.WithCompressConfig(cfg))
	if err != nil {
		return fmt.Errorf("store payload %s: %w", targetPath, err)
	}

	metadata := header.NewTemplateMetadata(buildID, blockSize, uint64(len(data)))

	spec, err := header.NewHeader(metadata, []header.BuildMap{{
		Offset:             0,
		Length:             uint64(len(data)),
		BuildId:            buildID,
		BuildStorageOffset: 0,
	}})
	if err != nil {
		return fmt.Errorf("build header: %w", err)
	}

	spec.SetBuild(buildID, header.BuildData{
		Size:      int64(len(data)),
		Checksum:  checksum,
		FrameData: fullFT.Table(),
	})

	if _, _, _, err := header.StoreHeader(ctx, provider, headerPath, spec.CloneForUpload(opts.targetHeaderVersion)); err != nil {
		return fmt.Errorf("store header: %w", err)
	}

	return nil
}

func runMigrate(ctx context.Context, opts options) error {
	if opts.targetHeaderVersion != header.MetadataVersionV4 && opts.targetHeaderVersion != header.MetadataVersionV5 {
		return fmt.Errorf("-target-header-version %d: only 4 (V4) and 5 (V5) are supported", opts.targetHeaderVersion)
	}

	// -rate above this bound would truncate its ticker interval to zero and
	// panic in time.NewTicker; refuse it loudly instead.
	if opts.rate < 0 || opts.rate > maxRatePerSecond {
		return fmt.Errorf("-rate %d: outside the supported range 0 (unbounded) to %d per second", opts.rate, maxRatePerSecond)
	}

	if opts.deleteSuperseded && opts.dryRun {
		log.Printf("migrate-builds: note: -delete-superseded with -dry-run reports the deletions without performing them")
	}

	cfg, reencode, err := targetCompressConfig(opts)
	if err != nil {
		return err
	}

	builds, err := buildIDs(opts)
	if err != nil {
		return err
	}

	spec, err := resolveStorageSpec(opts)
	if err != nil {
		return err
	}

	provider, err := storage.NewProvider(ctx, spec)
	if err != nil {
		return err
	}

	// The backend helper (listing, exact-key deletion) is only needed when a
	// deletion can actually happen; building it otherwise would reject backends
	// that are perfectly fine for a rewrite-only migration.
	var store objectStore

	if opts.deleteSuperseded && opts.confirm && !opts.dryRun {
		if store, err = storeFor(ctx, spec); err != nil {
			return err
		}
	}

	payloadPlan := "kept as-is"
	if reencode {
		payloadPlan = "re-encoded to " + cfg.CompressionType().String()
	}

	log.Printf("migrate-builds: mode=%s builds=%d target-header=v%d payload=%s dry-run=%v concurrency=%d rate=%d",
		opts.mode, len(builds), opts.targetHeaderVersion, payloadPlan, opts.dryRun, opts.concurrency, opts.rate)

	rep, err := newReporter(opts.reportPath)
	if err != nil {
		return err
	}

	defer rep.close()

	runErr := forEachArtifact(ctx, opts, builds, rep, func(ctx context.Context, build string, kind artifactKind) (*artifactOutcome, error) {
		buildID, err := uuid.Parse(build)
		if err != nil {
			return nil, fmt.Errorf("build %q: %w", build, err)
		}

		return migrateArtifact(ctx, provider, store, buildID, kind, opts, cfg, reencode)
	})

	if runErr != nil {
		return runErr
	}

	log.Printf("migrate-builds: done (%s)", rep.summary())

	// Per-artifact failures are outcomes, not run errors: without this check a
	// run whose artifacts all failed would still exit clean. Reconcile already
	// fails on incomplete artifacts; the migrate path must match it, and the
	// rehearsal leg reads the summary either way.
	if n := rep.count(actionFailed); n > 0 {
		return fmt.Errorf("%d artifact(s) failed", n)
	}

	return nil
}
