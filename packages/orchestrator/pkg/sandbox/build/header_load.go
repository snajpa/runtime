//go:build linux

package build

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const (
	loadV4InitialBackoff     = 100 * time.Millisecond
	loadV4MaxBackoff         = 5 * time.Second
	loadV4MaxTransientErrors = 3
)

// LivenessProbe answers whether the uploader that a cross-orchestrator header
// wait depends on is still alive. known=false marks an inconclusive answer
// (Redis unavailable, no lease in this fleet): the poll then keeps the plain
// budget and never fails a wait early. Waiters supply probes that read the
// uploader's lease key (see sandbox.Uploads.leaseProbe).
type LivenessProbe func(ctx context.Context) (alive bool, known bool)

// ErrUploaderGone reports that the wait's uploader disappeared: its lease was
// observed alive and then absent for the whole grace period while the header
// still did not appear in storage. The failed upload attempt is retryable —
// the bytes stay on the uploader's node until its own retry budget (or its
// node death) decides their fate.
var ErrUploaderGone = errors.New("uploader lease lost")

// The cadences below are package vars so tests can shorten them; only tests
// write them.
var (
	// loadV4ProbeInterval is how often the poll asks the liveness probe.
	loadV4ProbeInterval = 30 * time.Second
	// loadV4LeaseGoneGrace is how long the uploader must be observed gone
	// before the poll fails the wait. Deliberately several times the uploader
	// lease TTL (sandbox.uploadLeaseTTL, currently 60 s) so a single missed
	// refresh can never trip an early failure.
	loadV4LeaseGoneGrace = 3 * time.Minute
	// loadV4ProgressInterval is the cadence of the in-wait progress warning.
	loadV4ProgressInterval = 5 * time.Minute
)

// uploadHeaderPollWait measures how long the upload-side poll waited for the
// finalized header to become visible in storage. Result attribute is
// "ok" / "deadline_exceeded" / "transient_errors" / "ctx_cancelled" /
// "upload_failed" / "uploader_gone", file_type is memfile|rootfs.
var uploadHeaderPollWait = utils.Must(buildMeter.Int64Histogram(
	"orchestrator.storage.upload.header_poll_wait",
	metric.WithDescription("Duration of the upload-side wait for the finalized V4 header to appear"),
	metric.WithUnit("ms"),
))

// PollRemoteStorageForHeader polls storage for the post-upload V4 header for buildID/fileType.
// ErrObjectNotExist is retried until the budget expires; other LoadHeader
// errors are tolerated up to loadV4MaxTransientErrors consecutive occurrences
// (e.g. transient GCS hiccups during the rare window between the upload-done
// signal and object visibility) before giving up.
//
// hint is an optional accelerator. A nil error received on the channel says
// "the upload just finished, poll storage now"; a non-nil error says "the
// upload failed" and PollRemoteStorageForHeader returns it immediately without further polling.
// A nil channel never fires, so callers without hint plumbing fall through to
// the ticker-only path. budget bounds total wait time.
//
// liveness, when non-nil, is consulted on loadV4ProbeInterval: if the
// uploader's lease was observed alive and has then been gone for
// loadV4LeaseGoneGrace, the poll gives up with ErrUploaderGone instead of
// polling storage to the budget. A nil probe, or a probe that never reports
// the lease alive, keeps the plain budget semantics.
func PollRemoteStorageForHeader(
	ctx context.Context,
	store storage.StorageProvider,
	buildID uuid.UUID,
	t DiffType,
	hint <-chan error,
	budget time.Duration,
	liveness LivenessProbe,
) (*header.Header, error) {
	start := time.Now()
	result := "ok"
	defer func() {
		uploadHeaderPollWait.Record(ctx, time.Since(start).Milliseconds(), metric.WithAttributes(
			attribute.String("file_type", string(t)),
			attribute.String("result", result),
		))
	}()

	hdrPath := storage.Paths{BuildID: buildID.String()}.HeaderFile(string(t))
	deadline := time.Now().Add(budget)

	// A nil probe keeps the plain budget; normalize it so the loop stays
	// branch-free about liveness.
	probe := liveness
	if probe == nil {
		probe = func(context.Context) (bool, bool) { return false, false }
	}
	probeTicker := time.NewTicker(loadV4ProbeInterval)
	defer probeTicker.Stop()
	progressTicker := time.NewTicker(loadV4ProgressInterval)
	defer progressTicker.Stop()

	backoff := loadV4InitialBackoff
	transientErrs := 0
	var (
		sawAlive  bool
		goneSince time.Time
		lastAlive bool
		lastKnown bool
	)
	for {
		h, _, err := header.LoadHeader(ctx, store, hdrPath)
		if err == nil {
			return h, nil
		}
		if !errors.Is(err, storage.ErrObjectNotExist) {
			transientErrs++
			if transientErrs >= loadV4MaxTransientErrors {
				result = "transient_errors"

				return nil, fmt.Errorf("load V4 header for %s/%s after %d attempts: %w", buildID, t, transientErrs, err)
			}
		} else {
			transientErrs = 0
		}
		if !time.Now().Before(deadline) {
			result = "deadline_exceeded"

			return nil, fmt.Errorf("V4 header for %s/%s not visible after %s: %w", buildID, t, budget, err)
		}

		select {
		case <-ctx.Done():
			result = "ctx_cancelled"

			return nil, ctx.Err()
		case hintErr := <-hint:
			if hintErr != nil {
				result = "upload_failed"

				return nil, fmt.Errorf("upload signaled failure for %s/%s: %w", buildID, t, hintErr)
			}
			backoff = loadV4InitialBackoff
		case <-probeTicker.C:
			lastAlive, lastKnown = probe(ctx)
			switch {
			case lastAlive:
				sawAlive = true
				goneSince = time.Time{}
			case lastKnown && sawAlive:
				// Continuous corroborated absence only: an inconclusive
				// answer (below) restarts the confirmation window.
				if goneSince.IsZero() {
					goneSince = time.Now()
				}
				if time.Since(goneSince) >= loadV4LeaseGoneGrace {
					result = "uploader_gone"

					return nil, fmt.Errorf("uploader for %s/%s disappeared (lease gone for %s, header still not visible): %w", buildID, t, loadV4LeaseGoneGrace, ErrUploaderGone)
				}
			default:
				goneSince = time.Time{}
			}
		case <-progressTicker.C:
			logger.L().Warn(ctx, "cross-orchestrator header wait in progress",
				logger.WithBuildID(buildID.String()),
				zap.String("file_type", string(t)),
				zap.Duration("waited", time.Since(start)),
				zap.Bool("uploader_alive", lastAlive),
				zap.Bool("uploader_known", lastKnown),
			)
		case <-time.After(backoff):
			if backoff < loadV4MaxBackoff {
				backoff *= 2
			}
		}
	}
}
