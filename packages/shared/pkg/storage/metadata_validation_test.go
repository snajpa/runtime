package storage

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every provider rejects out-of-contract metadata before touching the backend,
// so the decision cannot depend on the selected backend (REQ-B1 parity,
// REQ-H1 bounds).
func TestProvidersRejectInvalidMetadata(t *testing.T) {
	t.Parallel()

	invalid := map[string]ObjectMetadata{
		"control byte":    {"k": "a\x00b"},
		"oversized key":   {strings.Repeat("k", MaxMetadataKeyBytes+1): "v"},
		"oversized value": {"k": strings.Repeat("v", MaxMetadataValueBytes+1)},
	}

	objects := []struct {
		name string
		obj  any
	}{
		{name: "fs", obj: &fsObject{path: filepath.Join(t.TempDir(), "obj")}},
		{name: "s3", obj: &awsObject{}},
		{name: "gcs", obj: &gcpObject{}},
		{name: "azure", obj: &azureObject{}},
	}

	for _, tt := range objects {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for reason, metadata := range invalid {
				t.Run(reason, func(t *testing.T) {
					t.Parallel()

					blob, ok := tt.obj.(Blob)
					require.True(t, ok)
					require.Error(t, blob.Put(t.Context(), []byte("x"), WithMetadata(metadata)))

					seekable, ok := tt.obj.(Seekable)
					require.True(t, ok)

					_, _, err := seekable.StoreFile(t.Context(), filepath.Join(t.TempDir(), "missing"), WithMetadata(metadata))
					require.Error(t, err)
				})
			}
		})
	}
}

// The filesystem provider ignores metadata storage but accepts valid metadata
// end to end.
func TestFSProviderAcceptsValidMetadata(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)
	obj, err := p.OpenBlob(t.Context(), "meta/object.bin")
	require.NoError(t, err)

	require.NoError(t, obj.Put(t.Context(), []byte("payload"), WithMetadata(ObjectMetadata{"team_id": "t1"})))

	got, err := GetBlob(t.Context(), obj)
	require.NoError(t, err)
	require.Equal(t, "payload", string(got))
}
