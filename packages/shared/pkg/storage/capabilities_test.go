package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// plainProvider implements StorageProvider but does not report capabilities.
type plainProvider struct{}

func (plainProvider) DeleteObjectsWithPrefix(context.Context, string) error { return nil }

func (plainProvider) UploadSignedURL(context.Context, string, time.Duration) (UploadURL, error) {
	return UploadURL{}, nil
}

func (plainProvider) OpenBlob(context.Context, string) (Blob, error) { return nil, nil }

func (plainProvider) OpenSeekable(context.Context, string) (Seekable, error) { return nil, nil }

func (plainProvider) GetDetails() string { return "plain" }

func TestCapabilitiesOf_Providers(t *testing.T) {
	t.Parallel()

	fsPlain := newFileSystemStorage("/tmp/capabilities", "", nil)
	fsSigning := newFileSystemStorage("/tmp/capabilities", "http://localhost:5008", []byte("hmac-key"))

	tests := []struct {
		name     string
		provider StorageProvider
		want     Capabilities
	}{
		{
			name:     "filesystem without upload endpoint",
			provider: fsPlain,
			want: Capabilities{
				Name:        "fs",
				AbortUpload: true,
			},
		},
		{
			name:     "filesystem with upload endpoint",
			provider: fsSigning,
			want: Capabilities{
				Name:            "fs",
				AbortUpload:     true,
				SignedUploadURL: true,
			},
		},
		{
			name:     "s3",
			provider: &awsStorage{},
			want: Capabilities{
				Name:                 "s3",
				DeleteBatchSize:      1000,
				AbortUpload:          true,
				SignedUploadURL:      true,
				Multipart:            true,
				MultipartMinPartSize: cloudMinPartSizeMB << 20,
			},
		},
		{
			name:     "gcs",
			provider: &gcpStorage{},
			want: Capabilities{
				Name:                 "gcs",
				DeleteBatchSize:      1,
				AbortUpload:          true,
				SignedUploadURL:      true,
				CustomMetadata:       true,
				Multipart:            true,
				MultipartMinPartSize: cloudMinPartSizeMB << 20,
			},
		},
		{
			name:     "azure",
			provider: &azureStorage{},
			want: Capabilities{
				Name:            "azure",
				DeleteBatchSize: 1,
				SignedUploadURL: true,
				CustomMetadata:  true,
				Multipart:       true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, CapabilitiesOf(tt.provider))
		})
	}
}

// Providers that do not report capabilities — and nil — get the conservative
// default: the least capable matrix, never an assumed feature.
func TestCapabilitiesOf_Fallback(t *testing.T) {
	t.Parallel()

	require.Equal(t, DefaultCapabilities, CapabilitiesOf(nil))
	require.Equal(t, DefaultCapabilities, CapabilitiesOf(plainProvider{}))

	require.Equal(t, "unknown", DefaultCapabilities.Name)
	require.Equal(t, 1, DefaultCapabilities.DeleteBatchSize)
	require.False(t, DefaultCapabilities.AbortUpload)
	require.False(t, DefaultCapabilities.SignedUploadURL)
	require.False(t, DefaultCapabilities.CustomMetadata)
	require.False(t, DefaultCapabilities.Multipart)
}

// The cache wrapper must not mask the wrapped provider's matrix.
func TestCapabilitiesOf_CacheDelegates(t *testing.T) {
	t.Parallel()

	inner := newFileSystemStorage("/tmp/capabilities", "http://localhost:5008", []byte("hmac-key"))
	wrapped := &cache{inner: inner}

	require.Equal(t, inner.Capabilities(), CapabilitiesOf(wrapped))
}

func TestCapabilityError(t *testing.T) {
	t.Parallel()

	missing := DefaultCapabilities

	for name, err := range map[string]error{
		"abort":    missing.RequireAbortUpload(),
		"signing":  missing.RequireSignedUploadURL(),
		"metadata": missing.RequireCustomMetadata(),
	} {
		require.ErrorIs(t, err, ErrCapabilityUnsupported, name)

		var capErr *CapabilityError
		require.ErrorAs(t, err, &capErr, name)
		require.Equal(t, missing.Name, capErr.Provider, name)
		require.NotEmpty(t, capErr.Capability, name)
	}

	capable := Capabilities{Name: "capable", AbortUpload: true, SignedUploadURL: true, CustomMetadata: true}
	require.NoError(t, capable.RequireAbortUpload())
	require.NoError(t, capable.RequireSignedUploadURL())
	require.NoError(t, capable.RequireCustomMetadata())
}
