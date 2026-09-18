//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jellydator/ttlcache/v3"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template/peerclient"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	errUploadInFlight  = errors.New("upload already in flight for build")
	ErrBuildNotInCache = errors.New("build not in template cache")
)

const (
	// futureTTL must outlive a parent upload's full retry window so a child's
	// in-memory Wait still finds the parent's future. Keep >= the upload retry
	// budget (server.uploadTotalBudget, 2h).
	futureTTL = 3 * time.Hour

	// refreshHeaderBudget bounds how long an upload Wait polls remote storage
	// for a parent's V4 header. Crosses orchestrators: A may still be uploading
	// on a remote orch when B's runV4 calls Wait(A) here. It must be >= the
	// parent's full retry window (server.uploadTotalBudget, 2h); otherwise the
	// poll's budget expiry returns a non-retryable "object does not exist" and
	// the child gives up while the parent is still retrying. The per-attempt
	// context (server.uploadTimeout) bounds the actual poll duration.
	refreshHeaderBudget = 2 * time.Hour

	// uploadDoneChannelPrefix is the Redis pub/sub channel prefix for per-build
	// upload-finished signals. Empty payload = success; non-empty = upload error.
	uploadDoneChannelPrefix = "orchestrator.upload.done." // followed by buildID String

	// uploadLeaseKeyPrefix is the Redis key prefix for the uploader lease: a
	// short-TTL, refreshed key that lets cross-orchestrator header waiters
	// tell a live upload apart from a node that died (S-40). It is separate
	// from the reader-facing peer routing key, whose semantics stay unchanged.
	uploadLeaseKeyPrefix = "orchestrator.upload.lease." // followed by buildID String
)

// The lease timings are package vars so tests can shorten them; only tests
// write them.
var (
	// uploadLeaseTTL bounds how long a waiter trusts an uploader that stops
	// refreshing. It is refreshed at one third of the TTL while the upload runs.
	uploadLeaseTTL = 60 * time.Second
	// uploadLeaseMaxLifetime caps the heartbeat when an upload never signals a
	// terminal outcome; it mirrors the upload retry window plus margin.
	uploadLeaseMaxLifetime = 2*time.Hour + 5*time.Minute
)

type templateLookup interface {
	GetCachedTemplate(buildID string) (template.Template, bool)
}

// Uploads is the in-flight upload table. Each entry's future fires when its
// build's V4 header has been swapped, gating child layers that depend on it.
//
// Cross-orch coordination uses Redis pub/sub on per-build channels: the
// uploader publishes on Finish, consumers subscribe inside Wait while polling
// remote storage. The Redis client is optional — nil falls back to ticker-only
// polling. While an upload is in flight, Uploads also keeps a short-TTL lease
// key fresh (startUploadLease); waiters read it through leaseProbe to bound
// their storage poll by the uploader's liveness instead of the flat budget.
//
// leaseStore is the narrow Redis surface the upload lease needs (Set/Get/Del).
// *redis.Client and the other UniversalClient implementations satisfy it; the
// interface keeps the heartbeat and its probe testable without a server.
type leaseStore interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

type Uploads struct {
	tc          templateLookup
	persistence storage.StorageProvider
	p2p         peerclient.Resolver
	redis       redis.UniversalClient
	// lease is the same Redis client narrowed for the upload lease. Nil when
	// Redis is not configured: the lease then no-ops and waiters keep the
	// plain budget (REQ-G4).
	lease leaseStore

	futures *ttlcache.Cache[uuid.UUID, *utils.ErrorOnce]
	// stopCh ends every lease heartbeat on Stop. Nil for Uploads values built
	// without NewUploads (tests); a nil channel never fires.
	stopCh chan struct{}
}

func NewUploads(tc *template.Cache, persistence storage.StorageProvider, p2p peerclient.Resolver, redisClient redis.UniversalClient) *Uploads {
	futures := ttlcache.New(
		ttlcache.WithTTL[uuid.UUID, *utils.ErrorOnce](futureTTL),
	)
	go futures.Start()

	var lease leaseStore
	if redisClient != nil {
		lease = redisClient
	}

	return &Uploads{
		tc:          tc,
		persistence: persistence,
		p2p:         p2p,
		redis:       redisClient,
		lease:       lease,
		futures:     futures,
		stopCh:      make(chan struct{}),
	}
}

func (u *Uploads) Stop() {
	u.futures.Stop()

	if u.stopCh != nil {
		select {
		case <-u.stopCh:
		default:
			close(u.stopCh)
		}
	}
}

// Start replaces a finished future at the same key; rejects an in-flight one.
// Build IDs are unique per upload so concurrent Starts for the same key are
// not expected — the in-flight check only guards against accidental misuse.
//
// ctx only seeds the lease heartbeat; the heartbeat is detached from its
// cancellation (see startUploadLease) and outlives the request that started
// the upload.
func (u *Uploads) Start(ctx context.Context, buildID uuid.UUID) (*utils.ErrorOnce, error) {
	if existing := u.futures.Get(buildID); existing != nil {
		select {
		case <-existing.Value().Done():
		default:
			return nil, fmt.Errorf("%w: %s", errUploadInFlight, buildID)
		}
	}

	fut := utils.NewErrorOnce()
	u.futures.Set(buildID, fut, ttlcache.DefaultTTL)
	// The lease spans the whole retry window: it is refreshed until the
	// terminal outcome is signalled (Finish sets the future).
	u.startUploadLease(ctx, buildID, fut.Done())

	return fut, nil
}

func uploadLeaseKey(buildID uuid.UUID) string {
	return uploadLeaseKeyPrefix + buildID.String()
}

// startUploadLease keeps the per-build lease key fresh for as long as the
// upload's future is pending, so cross-orchestrator waiters can tell a live
// upload apart from a node that died (S-40). No-op without Redis; the key's
// TTL covers a crashed process, and the heartbeat is bounded by
// uploadLeaseMaxLifetime even if a terminal outcome never arrives.
func (u *Uploads) startUploadLease(ctx context.Context, buildID uuid.UUID, done <-chan struct{}) {
	if u.lease == nil {
		return
	}

	// The lease must outlive the request that started the upload — the upload
	// itself runs detached the same way — so only the terminal outcome, Stop,
	// or the lifetime cap ends the heartbeat.
	go u.runUploadLease(context.WithoutCancel(ctx), buildID, done)
}

// runUploadLease is the heartbeat loop. It clears the key when the upload
// concludes, when the process stops its uploads, or at the lifetime cap.
func (u *Uploads) runUploadLease(ctx context.Context, buildID uuid.UUID, done <-chan struct{}) {
	key := uploadLeaseKey(buildID)

	refresh := uploadLeaseTTL / 3
	if refresh <= 0 {
		refresh = time.Millisecond
	}
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	maxLifetime := time.NewTimer(uploadLeaseMaxLifetime)
	defer maxLifetime.Stop()

	for {
		if err := u.lease.Set(ctx, key, "1", uploadLeaseTTL).Err(); err != nil {
			logger.L().Warn(ctx, "failed to refresh upload lease",
				logger.WithBuildID(buildID.String()),
				zap.Error(err),
			)
		}

		select {
		case <-done:
			u.clearUploadLease(ctx, key)

			return
		case <-u.stopCh:
			u.clearUploadLease(ctx, key)

			return
		case <-maxLifetime.C:
			u.clearUploadLease(ctx, key)

			return
		case <-ticker.C:
		}
	}
}

// clearUploadLease drops the lease; the TTL would cover it anyway, this just
// makes completion visible to waiters immediately.
func (u *Uploads) clearUploadLease(ctx context.Context, key string) {
	if err := u.lease.Del(ctx, key).Err(); err != nil {
		logger.L().Warn(ctx, "failed to clear upload lease",
			zap.String("key", key),
			zap.Error(err),
		)
	}
}

// leaseProbe answers the header poll's liveness question from the uploader's
// lease key. Nil when Redis is not configured: the poll then keeps the plain
// budget. An error reading the key is reported as inconclusive so a Redis
// problem can never fail a wait early.
func (u *Uploads) leaseProbe(buildID uuid.UUID) build.LivenessProbe {
	if u.lease == nil {
		return nil
	}

	key := uploadLeaseKey(buildID)

	return func(ctx context.Context) (alive, known bool) {
		_, err := u.lease.Get(ctx, key).Result()
		switch {
		case err == nil:
			return true, true
		case errors.Is(err, redis.Nil):
			return false, true
		default:
			return false, false
		}
	}
}

// Wait returns the parent's post-upload header, or (nil, nil) when the
// ancestor was never opened locally and no peer is mid-upload — the caller
// usually carries its BuildData through srcHeader.Builds already, and
// appendAncestorBuilds recovers it from the build's stored header otherwise.
func (u *Uploads) Wait(ctx context.Context, buildID uuid.UUID, t build.DiffType) (*header.Header, error) {
	ctx, span := tracer.Start(ctx, "wait-for-parent-upload", trace.WithAttributes(
		telemetry.WithBuildID(buildID.String()),
		attribute.String("file_type", string(t)),
	))
	defer span.End()

	d, err := u.find(ctx, buildID, t)
	if err != nil && !errors.Is(err, ErrBuildNotInCache) {
		logger.L().Warn(ctx, "ancestor resolution failed from cached template",
			logger.WithBuildID(buildID.String()),
			zap.String("file_type", string(t)),
			zap.Error(err),
		)

		return nil, err
	}

	if item := u.futures.Get(buildID); item != nil {
		if err := item.Value().WaitWithContext(ctx); err != nil {
			return nil, fmt.Errorf("wait for upload %s: %w", buildID, err)
		}
		if d == nil {
			return nil, fmt.Errorf("future fired but build %s not in template cache", buildID)
		}

		return d.Header(), nil
	}

	if d != nil && !d.Header().IncompletePendingUpload {
		return d.Header(), nil
	}

	if d == nil && !u.p2p.IsActive(buildID.String()) {
		return nil, nil
	}

	// P2P mid-upload. Poll remote storage, then swap onto the local device.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	h, err := build.PollRemoteStorageForHeader(ctx, u.persistence, buildID, t, u.subscribe(ctx, buildID), refreshHeaderBudget, u.leaseProbe(buildID))
	if err != nil {
		return nil, err
	}
	if d != nil {
		d.SwapHeader(h)
	}

	return h, nil
}

func (u *Uploads) find(ctx context.Context, buildID uuid.UUID, t build.DiffType) (block.ReadonlyDevice, error) {
	tpl, ok := u.tc.GetCachedTemplate(buildID.String())
	if !ok {
		return nil, fmt.Errorf("build %s: %w", buildID, ErrBuildNotInCache)
	}

	switch t {
	case build.Memfile:
		return tpl.Memfile(ctx)
	case build.Rootfs:
		return tpl.Rootfs()
	default:
		return nil, fmt.Errorf("unsupported file type: %s", t)
	}
}

// --- Cross-orch upload-done signaling (Redis pub/sub on per-build channels) ---

func uploadDoneChannel(buildID uuid.UUID) string {
	return uploadDoneChannelPrefix + buildID.String()
}

// publishUploadDoneToRedis broadcasts an upload-finished signal so cross-orch waiters can stop
// polling. Best-effort; failures fall through to the ticker poll. Empty
// payload = success; non-empty = the upload error message.
func (u *Uploads) publishUploadDoneToRedis(ctx context.Context, buildID uuid.UUID, uploadErr error) {
	if u.redis == nil {
		return
	}

	payload := ""
	if uploadErr != nil {
		payload = uploadErr.Error()
	}

	if err := u.redis.Publish(ctx, uploadDoneChannel(buildID), payload).Err(); err != nil {
		logger.L().Warn(ctx, "failed to publish upload-done signal",
			logger.WithBuildID(buildID.String()),
			zap.Error(err),
		)
	}
}

// subscribe opens a per-call SUBSCRIBE on buildID's upload-done channel and
// returns a channel that fires once with the upload outcome. The subscription
// is torn down when ctx cancels (caller must use a derived context). Returns
// a nil channel when Redis is not configured — nil channels never fire, so
// LoadV4 cleanly degrades to ticker-only polling.
func (u *Uploads) subscribe(ctx context.Context, buildID uuid.UUID) <-chan error {
	if u.redis == nil {
		return nil
	}

	out := make(chan error, 1)

	go func() {
		ps := u.redis.Subscribe(ctx, uploadDoneChannel(buildID))
		defer ps.Close()

		msg, err := ps.ReceiveMessage(ctx)
		if err != nil {
			return // ctx cancelled or connection error: silent (ticker covers)
		}

		var uploadErr error
		if msg.Payload != "" {
			uploadErr = errors.New(msg.Payload)
		}

		select {
		case out <- uploadErr:
		case <-ctx.Done():
		}
	}()

	return out
}
