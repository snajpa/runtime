//go:build linux

package build

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TestLocalDiffFileCloseToDiffRemovesPartialOnError verifies CloseToDiff does not
// leave the partial cache file behind when it fails to produce a usable Diff.
// Nothing registers such a file in the DiffStore, so a leaked orphan would sit in
// the cache dir unreclaimable by disk-pressure eviction until process restart.
func TestLocalDiffFileCloseToDiffRemovesPartialOnError(t *testing.T) {
	t.Parallel()

	f, err := NewLocalDiffFile(t.TempDir(), "build-test-id", Rootfs)
	require.NoError(t, err)

	// Non-empty so we're past the zero-size NoDiff branch and into materialization.
	_, err = f.File.WriteAt(make([]byte, 128), 0)
	require.NoError(t, err)

	cachePath := f.cachePath
	require.FileExists(t, cachePath)

	// Force a materialization failure: closing the fd makes the Sync inside
	// CloseToDiff fail, driving the error path.
	require.NoError(t, f.File.Close())

	diff, err := f.CloseToDiff(blockSize)
	require.Error(t, err)
	require.Nil(t, diff)
	require.NoFileExists(t, cachePath, "partial diff file must be removed on materialization failure")
}

// TestLocalDiffFileCloseToDiffRemovesEmptyCacheFile verifies the zero-size NoDiff
// branch removes the cache file: NoDiff owns no path, so nothing downstream can.
func TestLocalDiffFileCloseToDiffRemovesEmptyCacheFile(t *testing.T) {
	t.Parallel()

	f, err := NewLocalDiffFile(t.TempDir(), "build-test-id", Rootfs)
	require.NoError(t, err)

	cachePath := f.cachePath
	require.FileExists(t, cachePath)

	// Nothing written, so CloseToDiff takes the zero-size NoDiff branch.
	diff, err := f.CloseToDiff(blockSize)
	require.NoError(t, err)
	require.IsType(t, &NoDiff{}, diff)
	require.NoFileExists(t, cachePath, "empty diff cache file must be removed")
}

func TestNewLocalDiffFilePermissions(t *testing.T) {
	t.Parallel()

	f, err := NewLocalDiffFile(t.TempDir(), "build-perm-test", Rootfs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	info, err := os.Stat(f.cachePath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(storage.CacheFilePerm), info.Mode().Perm())
}

// nonPeekableDiffSource is a wrapped source that cannot answer residency
// queries, standing in for the file-backed *block.Cache, whose presence
// tracker is internal.
type nonPeekableDiffSource struct{}

var _ block.DiffSource = (*nonPeekableDiffSource)(nil)

func (p *nonPeekableDiffSource) ReadAt(_ []byte, _ int64) (int, error) { return 0, nil }
func (p *nonPeekableDiffSource) Slice(_, length int64) ([]byte, error) {
	return make([]byte, length), nil
}
func (p *nonPeekableDiffSource) Size() (int64, error)                    { return 0, nil }
func (p *nonPeekableDiffSource) FileSize(context.Context) (int64, error) { return 0, nil }
func (p *nonPeekableDiffSource) BlockSize() int64                        { return header.PageSize }
func (p *nonPeekableDiffSource) Path(context.Context) (string, error)    { return "", nil }
func (p *nonPeekableDiffSource) Close() error                            { return nil }

// peekableDiffSource adds the residency answer on top, standing in for the
// memfd-backed provisional source: mapped models whether the memfd is still
// held for serving.
type peekableDiffSource struct {
	nonPeekableDiffSource

	mapped bool
}

var _ block.CachePeeker = (*peekableDiffSource)(nil)

func (p *peekableDiffSource) IsCached(context.Context, int64, int64) bool { return p.mapped }

// localDiffIsCachedFixture wires the shape a resumed sandbox header has: a
// mapping that resolves to a local diff registered under a build id.
func localDiffIsCachedFixture(t *testing.T, src block.DiffSource) *File {
	t.Helper()

	store, err := NewDiffStore(
		mustParseCfg(t),
		flagsWithMaxBuildCachePercentage(t, 90),
		t.TempDir(),
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)

	buildID := uuid.New()
	diff, err := NewLocalDiffFromCache(GetDiffStoreKey(buildID.String(), Memfile), src)
	require.NoError(t, err)
	store.Add(diff)

	const size = 4096
	hdr, err := header.NewHeader(
		header.NewTemplateMetadata(uuid.New(), size, size),
		[]header.BuildMap{{Offset: 0, Length: size, BuildId: buildID}},
	)
	require.NoError(t, err)

	// Guard the fixture: the peek must resolve the mapping to the registered
	// build id, or IsCached would take the uuid.Nil "unbacked, resident"
	// shortcut and the assertions below would prove nothing.
	mapping, err := hdr.GetShiftedMapping(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, buildID, mapping.BuildId)

	m, err := blockmetrics.NewMetrics(noop.NewMeterProvider())
	require.NoError(t, err)

	return NewFile(hdr, store, Memfile, nil, m)
}

// TestLocalDiffPromotesCachePeekerResidency pins the S-35 residency contract:
// build.localDiff forwards IsCached to a wrapped source that can answer, so
// build.File.IsCached reports a provisional memfd-backed range as resident
// while it is mapped, and uncached after the source released it.
func TestLocalDiffPromotesCachePeekerResidency(t *testing.T) {
	t.Parallel()

	src := &peekableDiffSource{mapped: true}
	f := localDiffIsCachedFixture(t, src)

	require.True(t, f.IsCached(t.Context(), 0, 4096),
		"mapped provisional source must report the range resident")

	src.mapped = false
	require.False(t, f.IsCached(t.Context(), 0, 4096),
		"released provisional source must report the range uncached")
}

// TestLocalDiffWithoutCachePeekerReportsUncached pins the conservative side: a
// wrapped source that cannot answer residency queries reports uncached, so
// dedup best-effort stores the page as current instead of reading the base.
func TestLocalDiffWithoutCachePeekerReportsUncached(t *testing.T) {
	t.Parallel()

	f := localDiffIsCachedFixture(t, &nonPeekableDiffSource{})

	require.False(t, f.IsCached(t.Context(), 0, 4096))
}
