//go:build linux

package build

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// The storage-path tombstone contract (S-37, REQ-G1/INV-8): a recorded verdict
// fails reads closed only while it is the path this diff still serves, and
// metadata a backend cannot verify fails closed under enforcement. Both halves
// are the operator-visible promise the lifecycle design builds on, so they are
// pinned here instead of living only in the comments.

func TestSoftDeleteErrFailsClosedOnlyForTheActivePath(t *testing.T) {
	t.Parallel()

	b := &StorageDiff{buildID: "build", diffType: DiffType("memfile")}
	b.source.Store(&source{dataPath: "/objects/a"})

	require.NoError(t, b.softDeleteErr(), "no recorded verdict means no enforcement")

	active := "/objects/a"
	b.softDeletedPath.Store(&active)
	require.ErrorIs(t, b.softDeleteErr(), storage.ErrObjectSoftDeleted,
		"the tombstoned active path must fail closed")

	// A peer transition repoints the diff at another object; the recorded
	// verdict must stop applying. Comparing by path (not a bool) is what makes
	// the latch race-free.
	b.source.Store(&source{dataPath: "/objects/b"})
	require.NoError(t, b.softDeleteErr(), "a superseded verdict must not leak to the new path")
}

//nolint:paralleltest // toggles process-wide flag overrides
func TestSoftDeleteUnverifiableMetadataFailsClosedUnderEnforce(t *testing.T) {
	featureflags.OverrideBoolFlag(featureflags.StorageSoftDeleteCheckFlag, true)
	featureflags.OverrideBoolFlag(featureflags.StorageSoftDeleteEnforceFlag, true)
	t.Cleanup(func() {
		featureflags.OverrideBoolFlag(featureflags.StorageSoftDeleteCheckFlag, false)
		featureflags.OverrideBoolFlag(featureflags.StorageSoftDeleteEnforceFlag, false)
	})

	flags, err := featureflags.NewClient()
	require.NoError(t, err)

	// A blob that cannot answer custom metadata: no Metadata method, so
	// BlobCustomMetadata reports ErrMetadataUnsupported — the deterministic gap
	// that must fail closed under enforcement rather than serve a possibly
	// tombstoned layer.
	provider := storage.NewMockStorageProvider(t)
	provider.EXPECT().OpenBlob(mock.Anything, "/objects/a").Return(storage.NewMockBlob(t), nil)

	b := &StorageDiff{
		buildID:     "build",
		diffType:    DiffType("memfile"),
		persistence: provider,
	}
	b.source.Store(&source{dataPath: "/objects/a"})

	b.softDeleteCheck(t.Context(), flags)

	require.ErrorIs(t, b.softDeleteErr(), storage.ErrObjectSoftDeleted,
		"unverifiable metadata fails closed while enforcement is on")
}
