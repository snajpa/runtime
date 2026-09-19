package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// reconcile outcomes.
const (
	actionComplete       = "complete"
	actionMissingHeader  = "missing-header"
	actionMissingPayload = "missing-payload"
	actionMismatch       = "mismatch"
	actionOrphan         = "orphan"
	actionUnrecognised   = "unrecognised"
)

// runReconcile verifies that the objects a build's artifacts reference are the
// objects that are present, and - with -scan-prefix - reports objects that no
// artifact references. Nothing is deleted without -delete-orphans -confirm, and
// even then only objects that provably are superseded payloads of a healthy
// artifact.
func runReconcile(ctx context.Context, opts options) error {
	spec, err := resolveStorageSpec(opts)
	if err != nil {
		return err
	}

	provider, err := storage.NewProvider(ctx, spec)
	if err != nil {
		return err
	}

	// A prefix scan can run without a build list: it reports what is there.
	builds, err := buildIDs(opts)
	if err != nil {
		if opts.scanPrefix == "" {
			return err
		}

		builds = nil
	}

	rep, err := newReporter(opts.reportPath)
	if err != nil {
		return err
	}

	defer rep.close()

	incomplete := 0

	if len(builds) > 0 {
		for _, build := range builds {
			buildID, err := uuid.Parse(build)
			if err != nil {
				return fmt.Errorf("build %q: %w", build, err)
			}

			for _, kind := range artifactKinds() {
				outcome := reconcileArtifact(ctx, opts, provider, buildID, kind)
				if outcome.Action != actionComplete {
					incomplete++
				}

				rep.record(outcome)
			}
		}
	}

	if opts.scanPrefix != "" {
		if err := scanPrefix(ctx, opts, spec, provider, builds, rep); err != nil {
			return err
		}
	}

	log.Printf("reconcile: done (%s)", rep.summary())

	if incomplete > 0 {
		return fmt.Errorf("%d artifact(s) are not complete", incomplete)
	}

	return nil
}

// reconcileArtifact checks one artifact: the header it should have, and the
// payload that header describes. With -verify (the default) the payload is read
// and checked; without it only presence is reported.
func reconcileArtifact(ctx context.Context, opts options, provider storage.StorageProvider, buildID uuid.UUID, kind artifactKind) *artifactOutcome {
	paths := storage.Paths{BuildID: buildID.String()}

	outcome := &artifactOutcome{Build: buildID.String(), Artifact: kind.label, Action: actionComplete}

	loaded, _, err := header.LoadHeader(ctx, provider, paths.HeaderFile(kind.name))
	switch {
	case err != nil && errors.Is(err, storage.ErrObjectNotExist):
		outcome.Action = actionMissingHeader
		outcome.Detail = " header not present"

		return outcome
	case err != nil:
		outcome.Action = actionFailed
		outcome.Detail = " read header: " + err.Error()

		return outcome
	}

	outcome.FromHeaderVersion = loaded.Metadata.Version

	bd, ok := loaded.Builds[buildID]
	if !ok {
		outcome.Action = actionMissingHeader
		outcome.Detail = " header does not describe this build"

		return outcome
	}

	outcome.Bytes = bd.Size

	if !opts.verify {
		payload, err := firstExistingCandidate(ctx, provider, paths, kind.name)
		switch {
		case err != nil:
			outcome.Action = actionFailed
			outcome.Detail = " probe payload: " + err.Error()
		case payload == "":
			outcome.Action = actionMissingPayload
			outcome.Detail = " payload not present"
		default:
			outcome.FromPath = payload
			outcome.ToPath = payload
			outcome.Detail = " present (not verified)"
		}

		return outcome
	}

	resolution, err := resolvePayload(ctx, provider, paths, kind.name, bd, loaded.GetBuildFrameData(buildID), false, "")
	switch {
	case err != nil:
		outcome.Action = actionMismatch
		outcome.Detail = " " + err.Error()
		outcome.FromPath = resolution.Path
	case resolution.Path == "":
		outcome.Action = actionMissingPayload
		outcome.Detail = " payload not present"
	default:
		outcome.FromPath = resolution.Path
		outcome.ToPath = resolution.Path
		outcome.SupersededPaths = resolution.Superseded
	}

	return outcome
}

// scanPrefix lists the objects under a prefix and classifies them from what the
// listing actually contains: the header a build has, the payload that header
// describes and the small artifacts a build may carry are expected; a payload
// under a codec the header no longer reads is a superseded object a migration
// can remove (its size sidecar travels with it); anything else is reported but
// never deleted.
func scanPrefix(ctx context.Context, opts options, spec storage.Spec, provider storage.StorageProvider, builds []string, rep *reporter) error {
	store, err := storeFor(ctx, spec)
	if err != nil {
		return err
	}

	objects, err := store.List(ctx, opts.scanPrefix)
	if err != nil {
		return err
	}

	if len(builds) == 0 {
		// Derive the build set from the objects themselves.
		seen := map[string]struct{}{}

		for _, object := range objects {
			buildID, _ := storage.SplitPath(object.Key)
			if _, ok := seen[buildID]; ok {
				continue
			}

			seen[buildID] = struct{}{}
			builds = append(builds, buildID)
		}

		slices.Sort(builds)
	}

	var (
		referenced = map[string]struct{}{}
		live       = map[string]string{} // "{buildID}/{artifact}" -> live payload
		knownBuild = map[string]struct{}{}
	)

	for _, build := range builds {
		buildID, err := uuid.Parse(build)
		if err != nil {
			continue
		}

		knownBuild[buildID.String()] = struct{}{}

		paths := storage.Paths{BuildID: buildID.String()}

		// Small artifacts a build legitimately carries.
		referenced[paths.Snapfile()] = struct{}{}
		referenced[paths.Metadata()] = struct{}{}

		for _, kind := range artifactKinds() {
			headerPath := paths.HeaderFile(kind.name)
			referenced[headerPath] = struct{}{}

			loaded, _, err := header.LoadHeader(ctx, provider, headerPath)
			if err != nil {
				continue
			}

			bd, ok := loaded.Builds[buildID]
			if !ok {
				continue
			}

			resolution, resolutionErr := resolvePayload(ctx, provider, paths, kind.name, bd, loaded.GetBuildFrameData(buildID), false, "")
			if resolution.Path == "" {
				continue
			}

			referenced[resolution.Path] = struct{}{}
			referenced[storage.SizeSidecar(resolution.Path)] = struct{}{}
			live[buildID.String()+"/"+kind.name] = resolution.Path

			if resolutionErr != nil {
				rep.record(&artifactOutcome{
					Build: build, Artifact: kind.label, Action: actionMismatch,
					FromPath: resolution.Path, Detail: " " + resolutionErr.Error(),
				})
			}
		}
	}

	for _, object := range objects {
		if _, ok := referenced[object.Key]; ok {
			continue
		}

		buildID, file := storage.SplitPath(object.Key)

		// A size sidecar belongs to the payload it describes; it is removed with
		// it, so it is not reported on its own.
		if _, isSidecar := strings.CutSuffix(file, "."+storage.ObjectMetadataUncompressedSize); isSidecar {
			continue
		}

		artifactName := storage.StripCompression(file)

		switch {
		case artifactName != storage.MemfileName && artifactName != storage.RootfsName:
			rep.record(&artifactOutcome{
				Build: buildID, Artifact: path.Base(object.Key), Action: actionUnrecognised,
				FromPath: object.Key, Bytes: object.Size,
				Detail: " not a payload of a known artifact (never deleted)",
			})
		case !exists(knownBuild, buildID) || live[buildID+"/"+artifactName] == "":
			rep.record(&artifactOutcome{
				Build: buildID, Artifact: artifactName, Action: actionUnrecognised,
				FromPath: object.Key, Bytes: object.Size,
				Detail: " payload without a readable header (never deleted)",
			})
		default:
			livePath := live[buildID+"/"+artifactName]

			outcome := &artifactOutcome{
				Build: buildID, Artifact: artifactName, Action: actionOrphan,
				FromPath: object.Key, ToPath: livePath, Bytes: object.Size,
				SupersededPaths: []string{object.Key},
				Detail:          " superseded by " + livePath,
			}

			switch {
			case !opts.deleteOrphans:
				outcome.Detail += " (kept: pass -delete-orphans -confirm to remove)"
			case !opts.confirm || opts.dryRun:
				outcome.Detail += " (dry run: would be deleted)"
			default:
				if err := store.Delete(ctx, object.Key); err != nil {
					return err
				}

				outcome.Action = actionMigrate
				outcome.SupersededPaths = nil
				outcome.Detail = " superseded by " + livePath + "; deleted"
			}

			rep.record(outcome)
		}
	}

	return nil
}

// exists is a small readability helper for the scanned-build set.
func exists(set map[string]struct{}, key string) bool {
	_, ok := set[key]

	return ok
}
