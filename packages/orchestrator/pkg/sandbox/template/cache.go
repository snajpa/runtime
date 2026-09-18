//go:build linux

package template

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template/peerclient"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// CacheExpiration is how long to keep the template in the cache since the last
// access. Should be longer than the maximum possible sandbox lifetime. The
// orchestrator server's uploaded-build hint follows this window (S-38);
// long-running sandboxes can extend a specific entry beyond it (see
// getTemplateWithFetch).
const (
	CacheExpiration          = time.Hour * 25
	templateExpirationBuffer = time.Hour

	buildCacheTTL           = time.Hour * 25
	buildCacheDelayEviction = time.Second * 60
)

var (
	tracer     = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template")
	meter      = otel.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template")
	hitsMetric = utils.Must(meter.Int64Counter("orchestrator.templates.cache.hits",
		metric.WithDescription("Requests for templates that were already cached")))
	missesMetric = utils.Must(meter.Int64Counter("orchestrator.templates.cache.misses",
		metric.WithDescription("Requests for templates that were not cached")))
	memfileDedupDuration = utils.Must(telemetry.GetHistogram(meter, telemetry.OrchestratorSandboxMemfileDedupDurationName))
)

type Cache struct {
	config        cfg.Config
	flags         *featureflags.Client
	cache         *ttlcache.Cache[string, Template]
	persistence   storage.StorageProvider
	buildStore    *build.DiffStore
	blockMetrics  blockmetrics.Metrics
	rootCachePath string
	peers         peerclient.Resolver
	extendMu      sync.Mutex

	// fetch lifecycle: template fetches are detached from the request context
	// (another template may wait on the same fetch), so each runs on its own
	// cancelable ctx tracked here: Stop cancels them all and drains with a
	// bound instead of leaving them running into shutdown (S-19, INV-5).
	fetchMu       sync.Mutex
	fetchStopping bool
	fetchWG       sync.WaitGroup
	fetchCancels  map[uint64]context.CancelFunc
	nextFetchID   uint64
}

// templateFetchDrainTimeout bounds Stop's drain of in-flight template fetches:
// shutdown must not hang on a stuck fetch (S-19, INV-5). A var so tests can
// shorten it.
var templateFetchDrainTimeout = 30 * time.Second

// templateAccessorWaitTimeout bounds the waits of storage-template accessors
// whose signature has no context (Rootfs/Snapfile/UpdateMetadata): an
// unresolved fetch must not hang the caller forever (S-19, INV-5). A var so
// tests can shorten it.
var templateAccessorWaitTimeout = 2 * time.Minute

// waitBounded waits for p with a bound, for accessors that cannot take a
// caller context (S-19, INV-5).
func waitBounded[T any](p *utils.SetOnce[T]) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), templateAccessorWaitTimeout)
	defer cancel()

	return p.WaitWithContext(ctx)
}

// waitGroupBounded waits for wg within ctx.
func waitGroupBounded(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})

	go func() {
		defer close(done)

		wg.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startFetch runs fn on a cancelable context derived from the caller's via
// WithoutCancel: detached from the request's cancellation by design (another
// template may be waiting on the same fetch), but tracked so Stop can cancel
// and drain it with a bound (S-19, INV-5). Returns false once the cache is
// stopping: no new background work.
func (c *Cache) startFetch(parent context.Context, fn func(ctx context.Context)) bool {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()

	if c.fetchStopping {
		return false
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	id := c.nextFetchID
	c.nextFetchID++
	if c.fetchCancels == nil {
		c.fetchCancels = make(map[uint64]context.CancelFunc)
	}
	c.fetchCancels[id] = cancel

	c.fetchWG.Go(func() {
		defer func() {
			cancel()

			c.fetchMu.Lock()
			delete(c.fetchCancels, id)
			c.fetchMu.Unlock()
		}()

		fn(ctx)
	})

	return true
}

// stopFetches cancels in-flight template fetches and waits for them within
// templateFetchDrainTimeout: shutdown must not hang on a stuck fetch (S-19,
// INV-5). Returns the drain error, if any, for the caller to log.
func (c *Cache) stopFetches(ctx context.Context) error {
	c.fetchMu.Lock()
	c.fetchStopping = true

	cancels := make([]context.CancelFunc, 0, len(c.fetchCancels))
	for _, cancel := range c.fetchCancels {
		cancels = append(cancels, cancel)
	}
	c.fetchMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	ctx, cancel := context.WithTimeout(ctx, templateFetchDrainTimeout)
	defer cancel()

	return waitGroupBounded(ctx, &c.fetchWG)
}

// NewCache initializes a template new cache.
// It also deletes the old build cache directory content
// as it may contain stale data that are not managed by anyone.
func NewCache(
	config cfg.Config,
	flags *featureflags.Client,
	persistence storage.StorageProvider,
	metrics blockmetrics.Metrics,
	peers peerclient.Resolver,
) (*Cache, error) {
	cache := ttlcache.New(
		ttlcache.WithTTL[string, Template](CacheExpiration),
	)

	cache.OnEviction(func(ctx context.Context, _ ttlcache.EvictionReason, item *ttlcache.Item[string, Template]) {
		peers.Purge(item.Key())

		template := item.Value()

		err := template.Close(ctx)
		if err != nil {
			logger.L().Warn(ctx, "failed to cleanup template data", zap.String("item_key", item.Key()), zap.Error(err))
		}
	})

	// Delete the old build cache directory content.
	err := cleanDir(config.DefaultCacheDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove old build cache directory: %w", err)
	}

	buildStore, err := build.NewDiffStore(
		config,
		flags,
		config.DefaultCacheDir,
		buildCacheTTL,
		buildCacheDelayEviction,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create build store: %w", err)
	}

	return &Cache{
		blockMetrics:  metrics,
		config:        config,
		persistence:   persistence,
		buildStore:    buildStore,
		cache:         cache,
		flags:         flags,
		rootCachePath: config.BuilderConfig.SharedChunkCacheDir,
		peers:         peers,
	}, nil
}

func (c *Cache) Start(ctx context.Context) {
	c.buildStore.Start(ctx)

	go c.cache.Start()
}

func (c *Cache) Stop(ctx context.Context) {
	// Cancel and drain in-flight fetches first: they write into the build store
	// that Close tears down (S-19, INV-5). The drain inherits the caller's
	// shutdown budget instead of a detached background context.
	if err := c.stopFetches(ctx); err != nil {
		logger.L().Warn(ctx, "template fetch drain did not finish before shutdown", zap.Error(err))
	}

	c.buildStore.Close()
	c.cache.Stop()
	c.peers.Close()
}

func (c *Cache) Items() map[string]*ttlcache.Item[string, Template] {
	return c.cache.Items()
}

// LookupDiff returns the locally-cached diff for the given build and file name.
// Returns (nil, false) if the diff is not cached locally.
func (c *Cache) LookupDiff(buildID string, diffType build.DiffType) (build.Diff, bool) {
	key := build.GetDiffStoreKey(buildID, diffType)

	return c.buildStore.Lookup(key)
}

// Invalidate removes a template from the cache, forcing a refetch on next access.
func (c *Cache) Invalidate(buildID string) {
	c.cache.Delete(buildID)
}

// InvalidateAll clears all cached templates and build diffs.
// Used for cold start benchmarks to ensure no cached data is reused.
func (c *Cache) InvalidateAll() {
	c.cache.DeleteAll()
	c.buildStore.RemoveCache()
}

// GetTemplateOpts configures optional behavior for GetTemplate.
type GetTemplateOpts struct {
	MaxSandboxLengthHours int64
}

func (c *Cache) GetTemplate(
	ctx context.Context,
	buildID string,
	isSnapshot bool,
	isBuilding bool,
	opts ...GetTemplateOpts,
) (Template, error) {
	ctx, span := tracer.Start(ctx, "get template", trace.WithAttributes(
		attribute.Bool("is_snapshot", isSnapshot),
		attribute.Bool("is_building", isBuilding),
	))
	defer span.End()

	persistence := c.persistence
	// Because of the template caching, if we enable the NFS cache feature flag,
	// it will start working only for new orchestrators or new builds.
	if path, enabled := c.useNFSCache(ctx, isBuilding, isSnapshot); enabled {
		logger.L().Info(ctx, "using local template cache", zap.String("path", c.rootCachePath))
		persistence = storage.WrapInNFSCache(ctx, path, persistence, c.flags)
		span.SetAttributes(attribute.Bool("use_cache", true))
	} else {
		span.SetAttributes(attribute.Bool("use_cache", false))
	}

	// Wrap persistence with per-buildID peer routing.
	// Each layer's buildID is checked against Redis to find the source orchestrator.
	// This allows pulling data directly from the peer before GCS upload completes.
	if c.flags.BoolFlag(ctx, featureflags.PeerToPeerChunkTransferFlag) {
		persistence = peerclient.NewRoutingProvider(persistence, c.peers)
	}

	storageTemplate, err := newTemplateFromStorage(
		c.config.BuilderConfig,
		buildID,
		resolvedHeader(nil),
		resolvedHeader(nil),
		persistence,
		c.blockMetrics,
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create template cache from storage: %w", err)
	}

	var maxLen int64
	if len(opts) > 0 {
		maxLen = opts[0].MaxSandboxLengthHours
	}

	return c.getTemplateWithFetch(ctx, storageTemplate, maxLen), nil
}

func resolvedHeader(h *header.Header) *utils.SetOnce[*header.Header] {
	s := utils.NewSetOnce[*header.Header]()
	_ = s.SetValue(h)

	return s
}

func (c *Cache) AddSnapshot(
	ctx context.Context,
	buildId string,
	memfileHeader *utils.SetOnce[*header.Header],
	rootfsHeader *utils.SetOnce[*header.Header],
	localSnapfile File,
	localMetafile File,
	memfileDiff build.Diff,
	rootfsDiff build.Diff,
	// provisionalMemfileHeader/Diff: when non-nil the local memfile
	// template is built from the provisional header (which attributes dirty pages
	// to provisionalMemfileDiff's build id at identity offsets) so a concurrent
	// resume serves immediately from the memfd; once memfileHeader (the deduped
	// header) resolves it is swapped in. When nil, the template is built directly
	// from memfileHeader as before. The upload always uses memfileHeader.
	provisionalMemfileHeader *header.Header,
	provisionalMemfileDiff build.Diff,
	// provisionalMemfileCreatedAt, when non-zero, is the provisional header's
	// birth at pause; the swap goroutine records the dedup-duration metric
	// from it.
	provisionalMemfileCreatedAt time.Time,
	// provisionalSwapDone, when non-nil, is called once the deduped header has
	// been swapped in; it lets the dedup goroutine release the memfd the
	// provisional source was serving from.
	provisionalSwapDone func(),
) error {
	switch memfileDiff.(type) {
	case *build.NoDiff:
	default:
		c.buildStore.Add(memfileDiff)
	}
	if provisionalMemfileDiff != nil {
		if _, ok := provisionalMemfileDiff.(*build.NoDiff); !ok {
			// This provisional entry must stay resident until the SwapHeader below
			// (the provisional window). It is keyed by a synthetic build id with no
			// GCS object, so if it were evicted mid-window a resume read routed to
			// it would miss and couldn't be rebuilt (createDiff has nothing to
			// fetch), failing the read. It is pinned below (with the main memfile
			// diff) so disk-pressure eviction skips it for the window; TTL eviction
			// (hours) can't fire within the window (seconds).
			c.buildStore.Add(provisionalMemfileDiff)
		}
	}

	switch rootfsDiff.(type) {
	case *build.NoDiff:
	default:
		c.buildStore.Add(rootfsDiff)
	}

	// Build the local template from the provisional header (resolved now) so
	// Memfile() doesn't block on dedup; fall back to the deduped header future.
	// When serving a provisional header, pass the deduped header future as the
	// memfile's durable header so a pause parents off it, never the provisional
	// header (whose synthetic build id has no storage object). It is applied at
	// construction — before the memfile device is published — so no reader can
	// observe the device with the durable header unset.
	localMemfileHeader := memfileHeader
	var durableMemfileHeader *utils.SetOnce[*header.Header]
	if provisionalMemfileHeader != nil {
		localMemfileHeader = resolvedHeader(provisionalMemfileHeader)
		durableMemfileHeader = memfileHeader
	}

	storageTemplate, err := newTemplateFromStorage(
		c.config.BuilderConfig,
		buildId,
		localMemfileHeader,
		rootfsHeader,
		c.persistence,
		c.blockMetrics,
		localSnapfile,
		localMetafile,
		durableMemfileHeader,
	)
	if err != nil {
		// The swap goroutine below (which signals the release) is never spawned on
		// this early-return path, so signal here — otherwise the dedup goroutine
		// holds the provisional memfd for the full swap grace before releasing.
		if provisionalSwapDone != nil {
			provisionalSwapDone()
		}

		return fmt.Errorf("failed to create template cache from storage: %w", err)
	}

	// Use the template that is actually resident in the cache, not the local
	// storageTemplate: on a cache hit getTemplateWithFetch discards the local one
	// (never fetching it, so its memfile future never resolves) and returns the
	// pre-existing entry. The swap goroutine below must call Memfile on the
	// resident template — calling it on the discarded local instance would block
	// forever under swapCtx (no deadline), leaking the goroutine and its pins.
	cachedTemplate := c.getTemplateWithFetch(ctx, storageTemplate, 0)

	// Swap the provisional header for the deduped one once dedup finishes, so
	// subsequent reads route dirty pages to the (compacted) deduped diff and the
	// provisional memfd source is no longer referenced. The durable header was
	// wired in at construction above. On a cache hit the resident template was
	// built from its own header (not our provisional one), so SwapHeaderIfCurrent
	// below is a safe no-op there.
	if provisionalMemfileHeader != nil {
		// Pin both the main memfile diff and the provisional diff for the window.
		// They share a DedupedMemfdCache/memfd, but resume reads refresh only the
		// provisional entry, so disk-pressure eviction of either would break the
		// in-flight provisional serve: evicting the main entry Closes it, which
		// cancels dedup and tears down the shared memfd; evicting the provisional
		// entry makes a still-provisional-header read miss the store and fall
		// through to a storage fetch for the synthetic build id (never uploaded).
		// Pinning skips both in disk-pressure eviction (TTL still applies);
		// unpinned after the swap.
		var pinnedKeys []build.DiffStoreKey
		for _, d := range []build.Diff{memfileDiff, provisionalMemfileDiff} {
			if d == nil {
				continue
			}
			if _, isNoDiff := d.(*build.NoDiff); isNoDiff {
				continue
			}
			key := d.CacheKey()
			c.buildStore.Pin(key)
			pinnedKeys = append(pinnedKeys, key)
		}

		swapCtx := context.WithoutCancel(ctx)
		go func() {
			// Signal the dedup goroutine on every exit (success or the error
			// returns below) so it releases the memfd promptly. On an error the
			// swap can't happen and the resume is already broken, so nothing needs
			// the memfd kept mapped; without this the dedup goroutine would wait out
			// the full grace before releasing. Unpin the main diff on every exit too.
			if provisionalSwapDone != nil {
				defer provisionalSwapDone()
			}
			defer func() {
				for _, key := range pinnedKeys {
					c.buildStore.Unpin(key)
				}
			}()

			deduped, err := memfileHeader.Wait()
			if err != nil {
				logger.L().Warn(swapCtx, "provisional memfile header swap: deduped header failed", zap.Error(err))

				return
			}
			mem, err := cachedTemplate.Memfile(swapCtx)
			if err != nil {
				logger.L().Warn(swapCtx, "provisional memfile header swap: get memfile", zap.Error(err))

				return
			}
			if mem == nil {
				logger.L().Warn(swapCtx, "provisional memfile header swap: memfile is nil")

				return
			}
			// Swap only if the header is still the provisional one. Upload.publish
			// (and the P2P poll path) install a finalized header unconditionally;
			// if this goroutine runs late we must not clobber that newer header
			// with the older, still-incomplete deduped one. Either way the
			// provisional header is no longer needed, so release + drop below.
			if cas, ok := mem.(interface {
				SwapHeaderIfCurrent(old, next *header.Header) bool
			}); ok {
				if !cas.SwapHeaderIfCurrent(provisionalMemfileHeader, deduped) {
					logger.L().Info(swapCtx, "provisional memfile header swap: header already advanced; skipping")
				}
			} else {
				mem.SwapHeader(deduped)
			}

			if !provisionalMemfileCreatedAt.IsZero() {
				memfileDedupDuration.Record(swapCtx, time.Since(provisionalMemfileCreatedAt).Milliseconds())
			}

			// Reads now route off the provisional build id; the deferred signal
			// above lets the dedup goroutine release the memfd. The provisional
			// store entry is intentionally NOT deleted here: a reader that planned
			// on the provisional header but has not yet hit the store would miss and
			// fall through to a storage fetch for the synthetic build id (never
			// uploaded). It is harmless to leave — no reads route to it post-swap,
			// it reports FileSize 0 so it doesn't skew disk eviction, and the store
			// TTL reclaims it (its Close is a no-op).
		}()
	}

	return nil
}

// GetCachedTemplate returns the template for buildID if it is currently in the cache.
func (c *Cache) GetCachedTemplate(buildID string) (Template, bool) {
	item := c.cache.Get(buildID)
	if item == nil {
		return nil, false
	}

	return item.Value(), true
}

// UpdateMetadata overwrites the local metadata file for a cached template so that
// subsequent calls to Template.Metadata() on this node return the updated data
// (e.g. with freshly computed prefetch mappings) without requiring a cache
// invalidation or GCS round-trip.
func (c *Cache) UpdateMetadata(buildID string, meta metadata.Template) error {
	t, ok := c.GetCachedTemplate(buildID)
	if !ok {
		return fmt.Errorf("template %q not in cache", buildID)
	}

	return t.UpdateMetadata(meta)
}

func (c *Cache) useNFSCache(ctx context.Context, isBuilding bool, isSnapshot bool) (string, bool) {
	if isBuilding {
		// caching this layer doesn't speed up the next sandbox launch,
		// as the previous template isn't used to load the one that's being built.
		return "", false
	}

	var flagName featureflags.BoolFlag
	if isSnapshot {
		flagName = featureflags.SnapshotFeatureFlag
	} else {
		flagName = featureflags.TemplateFeatureFlag
	}

	useNFSCache := c.flags.BoolFlag(ctx, flagName)
	if useNFSCache {
		if c.rootCachePath == "" {
			logger.L().Warn(ctx, "NFSCache feature flag is enabled but cache path is not set")

			return "", false
		}
	}

	return c.rootCachePath, useNFSCache
}

func cleanDir(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("reading directory contents: %w", err)
	}

	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		if err := os.RemoveAll(entryPath); err != nil {
			return fmt.Errorf("removing %q: %w", entryPath, err)
		}
	}

	return nil
}

func (c *Cache) getTemplateWithFetch(ctx context.Context, tmpl *storageTemplate, maxSandboxLengthHours int64) Template {
	ttl := CacheExpiration
	if maxSandboxLengthHours > 0 {
		ttl = max(ttl, time.Duration(maxSandboxLengthHours)*time.Hour+templateExpirationBuffer)
	}

	key := tmpl.Files().CacheKey()

	c.extendMu.Lock()
	t, found := c.cache.GetOrSet(key, tmpl, ttlcache.WithTTL[string, Template](ttl))
	if found && t.TTL() < ttl {
		// Another team with a shorter max length cached this entry; extend it.
		c.cache.Set(key, t.Value(), ttl)
	}
	c.extendMu.Unlock()

	if !found {
		missesMetric.Add(ctx, 1)
		// The fetch is detached from the request context (another template may
		// be waiting on the same fetch), but it is tracked on the cache's own
		// fetch context so Stop can cancel and drain it with a bound (S-19).
		c.startFetch(ctx, func(fetchCtx context.Context) {
			tmpl.Fetch(fetchCtx, c.buildStore)
		})
	} else {
		hitsMetric.Add(ctx, 1)
	}

	return t.Value()
}
