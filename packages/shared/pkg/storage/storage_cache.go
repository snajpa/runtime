package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// CacheDirPerm and CacheFilePerm are the permissions policy for the local
// object caches: cache directories and files are private to the orchestrator
// user. Cache creation applies them explicitly (create + chmod) rather than
// relying on the create mode, which the umask filters and which existing
// files and directories ignore.
const (
	CacheDirPerm  = 0o700
	CacheFilePerm = 0o600
)

// skipCacheWritebackKeyType is the context key type for skipping NFS cache writeback.
type skipCacheWritebackKeyType struct{}

// WithSkipCacheWriteback returns a context that signals the NFS cache layer to
// skip writing fetched data back to the local cache. This is used by the
// prefetcher to avoid polluting the shared NFS cache with prefetch-specific reads.
func WithSkipCacheWriteback(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipCacheWritebackKeyType{}, true)
}

// skipCacheWriteback reports whether the context has the skip-cache-writeback flag set.
func skipCacheWriteback(ctx context.Context) bool {
	v, _ := ctx.Value(skipCacheWritebackKeyType{}).(bool)

	return v
}

type cache struct {
	rootPath  string
	chunkSize int64
	inner     StorageProvider
	flags     *featureflags.Client

	tracer trace.Tracer
}

var _ StorageProvider = (*cache)(nil)

func WrapInNFSCache(
	ctx context.Context,
	rootPath string,
	inner StorageProvider,
	flags *featureflags.Client,
) StorageProvider {
	cacheTracer := tracer

	createCacheSpans := flags.BoolFlag(ctx, featureflags.CreateStorageCacheSpansFlag)
	if !createCacheSpans {
		cacheTracer = noop.NewTracerProvider().Tracer("github.com/e2b-dev/infra/packages/shared/pkg/storage")
	}

	return &cache{
		rootPath:  rootPath,
		inner:     inner,
		chunkSize: MemoryChunkSize,
		flags:     flags,
		tracer:    cacheTracer,
	}
}

func (c cache) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	// no need to wait for cache deletion before returning
	go func(ctx context.Context) {
		c.deleteCachedObjectsWithPrefix(ctx, prefix)
	}(context.WithoutCancel(ctx))

	return c.inner.DeleteObjectsWithPrefix(ctx, prefix)
}

func (c cache) UploadSignedURL(ctx context.Context, path string, ttl time.Duration) (UploadURL, error) {
	return c.inner.UploadSignedURL(ctx, path, ttl)
}

func (c cache) OpenBlob(ctx context.Context, path string) (Blob, error) {
	innerObject, err := c.inner.OpenBlob(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to open object: %w", err)
	}

	localPath := filepath.Join(c.rootPath, path)
	if err = os.MkdirAll(localPath, CacheDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// MkdirAll leaves an existing directory untouched and always filters the
	// create mode through the umask, so enforce the cache policy explicitly.
	if err = os.Chmod(localPath, CacheDirPerm); err != nil {
		return nil, fmt.Errorf("failed to set cache directory permissions: %w", err)
	}

	return &cachedBlob{
		path:      localPath,
		chunkSize: c.chunkSize,
		inner:     innerObject,
		flags:     c.flags,
		tracer:    c.tracer,
	}, nil
}

func (c cache) OpenSeekable(ctx context.Context, path string) (Seekable, error) {
	innerObject, err := c.inner.OpenSeekable(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to open object: %w", err)
	}

	localPath := filepath.Join(c.rootPath, path)
	if err = os.MkdirAll(localPath, CacheDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// MkdirAll leaves an existing directory untouched and always filters the
	// create mode through the umask, so enforce the cache policy explicitly.
	if err = os.Chmod(localPath, CacheDirPerm); err != nil {
		return nil, fmt.Errorf("failed to set cache directory permissions: %w", err)
	}

	objType, _ := seekableObjectType(path)

	return &cachedSeekable{
		path:      localPath,
		chunkSize: c.chunkSize,
		inner:     innerObject,
		flags:     c.flags,
		tracer:    c.tracer,
		objType:   objType,
	}, nil
}

func (c cache) GetDetails() string {
	return fmt.Sprintf("[Caching file storage, base path set to %s, which wraps %s]",
		c.rootPath, c.inner.GetDetails())
}

// Capabilities implements CapabilityReporter by forwarding the wrapped
// provider's matrix — the cache layer does not change backend capabilities.
func (c cache) Capabilities() Capabilities {
	return CapabilitiesOf(c.inner)
}

func (c cache) deleteCachedObjectsWithPrefix(ctx context.Context, prefix string) {
	fullPrefix := filepath.Join(c.rootPath, prefix)
	if err := os.RemoveAll(fullPrefix); err != nil {
		logger.L().Error(ctx, "failed to remove object with prefix",
			zap.String("prefix", prefix),
			zap.String("path", fullPrefix),
			zap.Error(err))
	}
}

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}

	return err
}

// isCompleteRead reports whether a read of n bytes into a buffer of expected
// size represents a valid, cacheable result. A read is complete when either
// the full buffer was filled or io.EOF explains a non-empty short read (last chunk).
//
// Writeback callers pass err=nil: a streaming reader always ends in io.EOF
// regardless of whether the upstream was truncated, so the byte count is the
// only reliable signal that the captured bytes are safe to cache.
func isCompleteRead(n, expected int, err error) bool {
	return n == expected || (n > 0 && errors.Is(err, io.EOF))
}
