//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/google/uuid"
	"github.com/launchdarkly/go-sdk-common/v3/ldcontext"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	headers "github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

type Upload struct {
	buildID        uuid.UUID
	snap           *Snapshot
	paths          storage.Paths
	uploads        *Uploads
	store          storage.StorageProvider
	mem            storage.CompressConfig
	root           storage.CompressConfig
	useCase        string
	objectMetadata storage.ObjectMetadata
	future         *utils.ErrorOnce
	// framed forces the framed header path (V4/V5) for uncompressed uploads:
	// the per-file V4-for-uncompressed flags or V5 write selection set it.
	framed        bool
	headerVersion uint64
}

// headerWriteVersion selects the header format new uploads are written with.
//
// Policy (S-41, REQ-F2 "one write format going forward"): V5 is the write
// format. It ships behind the header-v5-write rollout flag so every reader in
// a mixed fleet understands V5 before writers produce it (read before write);
// until the flag is on, writes stay on the older formats — V4 for framed
// uploads, V3 when nothing is compressed and no framed flag is set. Enabling
// the flag makes V5 the only header format this writer emits. This function is
// the single decision point: flipping the default is a migration event, never
// an incidental change (see the S-41 roadmap note and S-55's matrix).
func headerWriteVersion(ctx context.Context, ff *featureflags.Client) uint64 {
	if ff != nil && ff.BoolFlag(ctx, featureflags.HeaderV5WriteFlag) {
		return headers.MetadataVersionV5
	}

	return headers.MetadataVersionV4
}

func NewUpload(
	ctx context.Context,
	uploads *Uploads,
	snap *Snapshot,
	store storage.StorageProvider,
	cfg storage.CompressConfig,
	ff *featureflags.Client,
	useCase string,
	objectMetadata storage.ObjectMetadata,
) (*Upload, error) {
	// Filesystem-only snapshots have no memfile (NoDiff, block size 0), so
	// resolving its compress config would fail validation ("block size must be
	// positive"). The memfile body and header are never uploaded anyway.
	var mem storage.CompressConfig
	var memV4 bool
	var err error
	if !snap.FilesystemSnapshot {
		mem, memV4, err = resolveCompressConfig(ctx, cfg, ff, storage.MemfileName, snap.MemorySnapshot.BlockSize, useCase)
		if err != nil {
			return nil, fmt.Errorf("resolve memfile compress config: %w", err)
		}
	}
	root, rootV4, err := resolveCompressConfig(ctx, cfg, ff, storage.RootfsName, snap.RootfsBlockSize, useCase)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs compress config: %w", err)
	}

	if useCase != "" {
		ctx = featureflags.AddToContext(ctx, featureflags.CompressUseCaseContext(useCase))
	}
	headerVersion := headerWriteVersion(ctx, ff)

	u := &Upload{
		buildID:        snap.BuildID,
		snap:           snap,
		paths:          storage.Paths{BuildID: snap.BuildID.String()},
		uploads:        uploads,
		store:          store,
		mem:            mem,
		root:           root,
		useCase:        useCase,
		objectMetadata: objectMetadata,
		framed:         memV4 || rootV4 || headerVersion == headers.MetadataVersionV5,
		headerVersion:  headerVersion,
	}

	if uploads != nil {
		fut, err := uploads.Start(ctx, snap.BuildID)
		if err != nil {
			return nil, err
		}
		u.future = fut
	}

	return u, nil
}

// layerSizeMetadata adds the layer's logical, mapped, and diff sizes (all
// uncompressed, from the diff header) to the base object metadata. They live on
// the data object because the memfile values depend on the async dedup header.
func (u *Upload) layerSizeMetadata(h *headers.Header) storage.ObjectMetadata {
	md := maps.Clone(u.objectMetadata)
	if md == nil {
		md = make(storage.ObjectMetadata)
	}
	if h == nil || h.Metadata == nil {
		return md
	}

	bytesByBuild := h.Mapping.BytesByBuild()
	var mapped uint64
	for _, b := range bytesByBuild {
		mapped += b
	}
	md[storage.ObjectMetadataLogicalSize] = strconv.FormatUint(h.Metadata.Size, 10)
	md[storage.ObjectMetadataMappedSize] = strconv.FormatUint(mapped, 10)
	md[storage.ObjectMetadataDiffSize] = strconv.FormatUint(bytesByBuild[h.Metadata.BuildId], 10)

	return md
}

func (u *Upload) Run(ctx context.Context) error {
	// Attach the upload use case so flag reads can target it (e.g. write-through only for builds).
	ctx = featureflags.AddToContext(ctx, featureflags.CompressUseCaseContext(u.useCase))

	// runV3 is the legacy unframed write path (V3); it remains only as the
	// rollback window and is a removal target per the S-41 roadmap.
	if !u.mem.IsCompressionEnabled() && !u.root.IsCompressionEnabled() && !u.framed {
		return u.runV3(ctx)
	}

	return u.runV4(ctx)
}

// Wait blocks until the upload has reached its terminal outcome (the future set
// by Finish) or ctx is done, returning the upload error. It lets a caller order
// work after the snapshot has durably landed — e.g. re-uploading the metadata
// object without racing the upload's own metadata write.
func (u *Upload) Wait(ctx context.Context) error {
	if u.future == nil {
		return nil
	}

	return u.future.WaitWithContext(ctx)
}

// Finish signals the upload's terminal outcome. Same-orch waiters wake on the
// future; cross-orch waiters wake on the Redis hint published here.
func (u *Upload) Finish(ctx context.Context, uploadErr error) {
	if u.future != nil {
		_ = u.future.SetError(uploadErr)
	}
	if u.uploads != nil {
		u.uploads.publishUploadDoneToRedis(ctx, u.buildID, uploadErr)
	}
}

// publish swaps a finalized header into the local cached device so peers and
// Wait()ers see the build as complete. ErrBuildNotInCache is the one acceptable
// failure mode: nothing was cached locally, nothing to swap.
func (u *Upload) publish(ctx context.Context, t build.DiffType, h *headers.Header) error {
	if u.uploads == nil {
		return nil
	}

	dev, err := u.uploads.find(ctx, u.buildID, t)
	if errors.Is(err, ErrBuildNotInCache) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load %s for swap: %w", t, err)
	}

	dev.SwapHeader(h)

	return nil
}

// resolveCompressConfig returns the effective compression config for a given
// file type and use case, plus whether the V4 header layout should be used for
// an uncompressed upload. Feature flags override the base config when active.
// Returns zero-value CompressConfig when compression is disabled. fileType,
// useCase are added to the LD evaluation context; blockSize constrains legal
// frame sizes — see storage.CompressConfig.ValidateFrameSize.
func resolveCompressConfig(ctx context.Context, base storage.CompressConfig, ff *featureflags.Client, fileType string, blockSize uint64, useCase string) (storage.CompressConfig, bool, error) {
	resolved := base
	var useV4 bool

	if ff != nil {
		var extra []ldcontext.Context
		if fileType != "" {
			extra = append(extra, featureflags.CompressFileTypeContext(fileType))
		}
		if useCase != "" {
			extra = append(extra, featureflags.CompressUseCaseContext(useCase))
		}
		ctx = featureflags.AddToContext(ctx, extra...)

		useV4 = ff.BoolFlag(ctx, featureflags.V4HeaderForUncompressedFlag)

		v := ff.JSONFlag(ctx, featureflags.CompressConfigFlag).AsValueMap()
		if v.Get("compressBuilds").BoolValue() {
			ct := v.Get("compressionType").StringValue()
			ldCfg := storage.CompressConfig{
				Enabled:            true,
				Type:               ct,
				Level:              v.Get("compressionLevel").IntValue(),
				FrameSizeKB:        v.Get("frameSizeKB").IntValue(),
				MinPartSizeMB:      v.Get("minPartSizeMB").IntValue(),
				FrameEncodeWorkers: v.Get("frameEncodeWorkers").IntValue(),
				EncoderConcurrency: v.Get("encoderConcurrency").IntValue(),
			}
			// Validate before the shortcut below: an unknown type used to be
			// discarded silently, leaving the base config in place.
			if err := ldCfg.Validate(); err != nil {
				return storage.CompressConfig{}, false, fmt.Errorf("compress-config flag: %w", err)
			}
			if ldCfg.CompressionType() != storage.CompressionNone {
				resolved = ldCfg
			}
		}
	}

	// A configuration that asks for compression but names a type this build
	// cannot produce is a misconfiguration, not a disabled config.
	if err := resolved.Validate(); err != nil {
		return storage.CompressConfig{}, false, err
	}

	if !resolved.IsCompressionEnabled() {
		return storage.CompressConfig{}, useV4, nil
	}

	if err := resolved.ValidateFrameSize(blockSize); err != nil {
		return storage.CompressConfig{}, false, err
	}

	return resolved, useV4, nil
}
