package storage

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifySHA256(t *testing.T) {
	t.Parallel()

	data := []byte("payload bytes")
	want := sha256.Sum256(data)

	require.NoError(t, VerifySHA256("obj", bytes.NewReader(data), want))

	corrupt := []byte("corrupted bytes")

	err := VerifySHA256("obj", bytes.NewReader(corrupt), want)
	require.ErrorIs(t, err, ErrDigestMismatch)

	var mismatch *DigestMismatchError
	require.ErrorAs(t, err, &mismatch)
	require.Equal(t, "obj", mismatch.Path)
	require.Equal(t, want, mismatch.Expected)
	require.Equal(t, sha256.Sum256(corrupt), mismatch.Actual)

	require.ErrorIs(t, VerifySHA256("obj", bytes.NewReader(data), [32]byte{}), ErrDigestUnknown)
}

// A stored compressed object verifies against the checksum the upload returned
// and fails closed once the stored bytes are corrupted (REQ-A2).
func TestVerifyObjectRoundTrip(t *testing.T) {
	t.Parallel()

	p := newTempProvider(t)

	data := generateSemiRandomData(2 * megabyte)
	inputPath := filepath.Join(t.TempDir(), "input.bin")
	require.NoError(t, os.WriteFile(inputPath, data, 0o600))

	obj, err := p.OpenSeekable(t.Context(), "layer/data.bin")
	require.NoError(t, err)

	ft, checksum, err := obj.StoreFile(t.Context(), inputPath, WithCompressConfig(defaultCfg(CompressionZstd, 2, 512*1024)))
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(data), checksum)

	require.NoError(t, VerifyObject(t.Context(), p, "layer/data.bin", ft.Table(), checksum))

	// An uncompressed object records no digest; a caller-supplied digest still
	// verifies.
	const rawPath = "layer/raw.bin"

	rawObj, err := p.OpenSeekable(t.Context(), rawPath)
	require.NoError(t, err)

	_, rawChecksum, err := rawObj.StoreFile(t.Context(), inputPath)
	require.NoError(t, err)
	require.Equal(t, [32]byte{}, rawChecksum, "an uncompressed store records no digest")
	require.NoError(t, VerifyObject(t.Context(), p, rawPath, nil, sha256.Sum256(data)))

	// Corrupting the stored compressed bytes must fail verification — either
	// the frame decode or the digest comparison catches it, never silent data.
	storedPath := filepath.Join(p.basePath, "layer", "data.bin")

	compressed, err := os.ReadFile(storedPath)
	require.NoError(t, err)

	compressed[len(compressed)/2] ^= 0xff
	require.NoError(t, os.WriteFile(storedPath, compressed, 0o600))

	err = VerifyObject(t.Context(), p, "layer/data.bin", ft.Table(), checksum)
	require.Error(t, err)
}
