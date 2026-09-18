package storageopts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObjectMetadataValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		md      ObjectMetadata
		wantErr bool
	}{
		{name: "nil is valid", md: nil},
		{name: "typical keys", md: ObjectMetadata{"team_id": "t1", "uncompressed-size": "123"}},
		{name: "empty key", md: ObjectMetadata{"": "v"}, wantErr: true},
		{name: "oversized key", md: ObjectMetadata{strings.Repeat("k", MaxMetadataKeyBytes+1): "v"}, wantErr: true},
		{name: "oversized value", md: ObjectMetadata{"k": strings.Repeat("v", MaxMetadataValueBytes+1)}, wantErr: true},
		{name: "control byte", md: ObjectMetadata{"k": "a\x00b"}, wantErr: true},
		{name: "invalid utf-8", md: ObjectMetadata{"k": "a\xffb"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.md.Validate()
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
		})
	}
}

// The total bound rejects many individually-valid entries.
func TestObjectMetadataValidateTotalBound(t *testing.T) {
	t.Parallel()

	md := make(ObjectMetadata)
	for i := range (MaxMetadataBytes / MaxMetadataValueBytes) + 1 {
		md[fmt.Sprintf("key-%d", i)] = strings.Repeat("v", MaxMetadataValueBytes)
	}

	require.Error(t, md.Validate())
}

func TestObjectMetadataWithUncompressedSize(t *testing.T) {
	t.Parallel()

	base := ObjectMetadata{"team_id": "t1"}
	withSize := base.WithUncompressedSize(1234)
	require.Equal(t, "1234", withSize[ObjectMetadataUncompressedSize])
	require.Equal(t, "t1", withSize["team_id"])
	require.NotContains(t, base, ObjectMetadataUncompressedSize, "the source map must not be mutated")

	size, ok := withSize.UncompressedSize()
	require.True(t, ok)
	require.Equal(t, int64(1234), size)
}
