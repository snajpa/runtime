//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/build"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	headers "github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// stubDiff is the minimal build.Diff a V3 upload consumes — only CachePath is
// used by the upload path. The read methods panic so an accidental use in the
// upload path is loud rather than silent.
type stubDiff struct{ path string }

func (d stubDiff) Close() error { return nil }

func (d stubDiff) ReadAt(context.Context, []byte, int64, *storage.FrameTable) (int, error) {
	panic("stubDiff.ReadAt must not be called by the upload path")
}

func (d stubDiff) Slice(context.Context, int64, int64, *storage.FrameTable) ([]byte, error) {
	panic("stubDiff.Slice must not be called by the upload path")
}

func (d stubDiff) CacheKey() build.DiffStoreKey { return build.DiffStoreKey("stub") }

func (d stubDiff) CachePath(context.Context) (string, error) { return d.path, nil }

func (d stubDiff) Size(context.Context) (int64, error) { return 0, nil }

func (d stubDiff) FileSize(context.Context) (int64, error) { return 0, nil }

func (d stubDiff) BlockSize() int64 { return 4096 }

// stubFile is the minimal template.File (Path + Close).
type stubFile struct{ path string }

func (f stubFile) Path() string { return f.path }

func (f stubFile) Close() error { return nil }

// recordingProvider wraps a real provider, records every blob/seekable open in
// order (the upload paths write through exactly these calls) and can hold one
// chosen object open so a test can observe the commit window while a body
// upload is still in flight.
type recordingProvider struct {
	storage.StorageProvider

	mu  sync.Mutex
	ops []string

	blockPath   string
	blocked     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
	releaseOnce sync.Once
}

func newRecordingProvider(base storage.StorageProvider, blockPath string) *recordingProvider {
	return &recordingProvider{
		StorageProvider: base,
		blockPath:       blockPath,
		blocked:         make(chan struct{}),
		release:         make(chan struct{}),
	}
}

func (p *recordingProvider) releaseBlock() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *recordingProvider) record(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ops = append(p.ops, path)
}

func (p *recordingProvider) opsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.ops...)
}

func (p *recordingProvider) opened(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Contains(p.ops, path)
}

func (p *recordingProvider) maybeBlock(path string) {
	if p.blockPath == "" || path != p.blockPath {
		return
	}
	p.blockOnce.Do(func() {
		close(p.blocked)
		<-p.release
	})
}

func (p *recordingProvider) OpenBlob(ctx context.Context, path string) (storage.Blob, error) {
	p.record(path)
	p.maybeBlock(path)

	return p.StorageProvider.OpenBlob(ctx, path)
}

func (p *recordingProvider) OpenSeekable(ctx context.Context, path string) (storage.Seekable, error) {
	p.record(path)
	p.maybeBlock(path)

	return p.StorageProvider.OpenSeekable(ctx, path)
}

func writeTestFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{0xAB}, size), 0o600))

	return path
}

// newV3UploadForTest builds an Upload that takes the legacy V3 path (no
// compression, no V4-for-uncompressed flag) over stub diffs and real temp
// files, plus the storage paths it will write.
func newV3UploadForTest(t *testing.T, store storage.StorageProvider, memSrc, rootSrc, snapSrc, metaSrc string) (*Upload, storage.Paths) {
	t.Helper()

	buildID := uuid.New()
	paths := storage.Paths{BuildID: buildID.String()}
	snap := &Snapshot{
		BuildID: buildID,
		MemorySnapshot: MemorySnapshot{
			Diff:       stubDiff{path: memSrc},
			DiffHeader: NewResolvedDiffHeader(&headers.Header{Metadata: &headers.Metadata{Version: 3}}),
			BlockSize:  4096,
		},
		RootfsBlockSize:  4096,
		RootfsDiff:       stubDiff{path: rootSrc},
		RootfsDiffHeader: NewResolvedDiffHeader(&headers.Header{Metadata: &headers.Metadata{Version: 3}}),
		Snapfile:         stubFile{path: snapSrc},
		Metafile:         stubFile{path: metaSrc},
	}

	u, err := NewUpload(t.Context(), nil, snap, store, storage.CompressConfig{}, nil, storage.UseCaseBuild, nil)
	require.NoError(t, err)

	return u, paths
}

// INV-2 regression: a finalized header must not become visible while a body
// upload is still in flight, and the final write order must place every data
// object before both headers. Pre-fix, headers and bodies shared one errgroup,
// so the header was written while the body was still uploading.
func TestRunV3_HeaderVisibleOnlyAfterBodies(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	memSrc := writeTestFile(t, src, "mem.bin", 8*1024)
	rootSrc := writeTestFile(t, src, "root.bin", 4*1024)
	snapSrc := writeTestFile(t, src, "snap.bin", 64)
	metaSrc := writeTestFile(t, src, "meta.bin", 64)

	base, err := storage.NewProvider(t.Context(), storage.Spec{Provider: storage.LocalStorageProvider, BasePath: t.TempDir()})
	require.NoError(t, err)

	u, paths := newV3UploadForTest(t, base, memSrc, rootSrc, snapSrc, metaSrc)

	rec := newRecordingProvider(base, paths.Memfile())
	u.store = rec
	t.Cleanup(rec.releaseBlock)

	errCh := make(chan error, 1)
	go func() { errCh <- u.Run(t.Context()) }()

	select {
	case <-rec.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("memfile body upload never reached the provider")
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.False(t, rec.opened(paths.MemfileHeader()) || rec.opened(paths.RootfsHeader()),
			"a finalized header became visible while a body upload was still in flight: %v", rec.opsSnapshot())
		time.Sleep(10 * time.Millisecond)
	}

	rec.releaseBlock()
	require.NoError(t, <-errCh)

	ops := rec.opsSnapshot()

	lastData := -1
	for _, p := range []string{paths.Memfile(), paths.Rootfs(), paths.Snapfile(), paths.Metadata()} {
		i := slices.Index(ops, p)
		require.NotEqual(t, -1, i, "data object %s missing from writes: %v", p, ops)
		lastData = max(lastData, i)
	}
	for _, p := range []string{paths.MemfileHeader(), paths.RootfsHeader()} {
		i := slices.Index(ops, p)
		require.NotEqual(t, -1, i, "header object %s missing from writes: %v", p, ops)
		require.Greater(t, i, lastData, "header %s was written before every data object: %v", p, ops)
	}
}

// REQ-A1 regression: a failed data phase must not publish any header. Pre-fix
// the header goroutine ran concurrently with the failing body upload.
func TestRunV3_BodyFailureWritesNoHeader(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	rootSrc := writeTestFile(t, src, "root.bin", 4*1024)
	snapSrc := writeTestFile(t, src, "snap.bin", 64)
	metaSrc := writeTestFile(t, src, "meta.bin", 64)
	missingMem := filepath.Join(src, "missing-mem.bin")

	base, err := storage.NewProvider(t.Context(), storage.Spec{Provider: storage.LocalStorageProvider, BasePath: t.TempDir()})
	require.NoError(t, err)

	rec := newRecordingProvider(base, "")
	u, paths := newV3UploadForTest(t, rec, missingMem, rootSrc, snapSrc, metaSrc)

	require.Error(t, u.Run(t.Context()))
	require.False(t, rec.opened(paths.MemfileHeader()),
		"memfile header written despite a failed body phase: %v", rec.opsSnapshot())
	require.False(t, rec.opened(paths.RootfsHeader()),
		"rootfs header written despite a failed body phase: %v", rec.opsSnapshot())
}

// The success path still writes exactly the six snapshot objects.
func TestRunV3_SuccessWritesAllObjects(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	memSrc := writeTestFile(t, src, "mem.bin", 8*1024)
	rootSrc := writeTestFile(t, src, "root.bin", 4*1024)
	snapSrc := writeTestFile(t, src, "snap.bin", 64)
	metaSrc := writeTestFile(t, src, "meta.bin", 64)

	base, err := storage.NewProvider(t.Context(), storage.Spec{Provider: storage.LocalStorageProvider, BasePath: t.TempDir()})
	require.NoError(t, err)

	rec := newRecordingProvider(base, "")
	u, paths := newV3UploadForTest(t, rec, memSrc, rootSrc, snapSrc, metaSrc)

	require.NoError(t, u.Run(t.Context()))

	ops := rec.opsSnapshot()
	require.Len(t, ops, 6, "unexpected write set: %v", ops)
	for _, p := range []string{
		paths.Memfile(), paths.Rootfs(), paths.Snapfile(), paths.Metadata(),
		paths.MemfileHeader(), paths.RootfsHeader(),
	} {
		require.Equal(t, 1, countOf(ops, p), "object %s must be written exactly once: %v", p, ops)
	}
}

func countOf(ops []string, path string) int {
	n := 0
	for _, op := range ops {
		if op == path {
			n++
		}
	}

	return n
}
