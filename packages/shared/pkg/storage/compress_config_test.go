package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCompressionType(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name string
		in   string
		want CompressionType
	}{
		{name: "empty means none", in: "", want: CompressionNone},
		{name: "none", in: "none", want: CompressionNone},
		{name: "none case-insensitive", in: "None", want: CompressionNone},
		{name: "lz4", in: "lz4", want: CompressionLZ4},
		{name: "lz4 case-insensitive", in: "LZ4", want: CompressionLZ4},
		{name: "lz4 padded", in: " lz4 ", want: CompressionLZ4},
		{name: "zstd", in: "zstd", want: CompressionZstd},
		{name: "zstd case-insensitive", in: "ZSTD", want: CompressionZstd},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseCompressionType(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	invalid := []string{"zstandard", "gzip", "nonee", "zstd x", "zstd;lz4"}
	for _, in := range invalid {
		t.Run("invalid "+in, func(t *testing.T) {
			t.Parallel()

			got, err := ParseCompressionType(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), in)
			assert.Equal(t, CompressionNone, got)
		})
	}
}

func TestCompressionTypeValidAndRoundTrip(t *testing.T) {
	t.Parallel()

	for _, ct := range []CompressionType{CompressionNone, CompressionZstd, CompressionLZ4} {
		assert.True(t, ct.Valid(), "%s must be valid", ct)

		got, err := ParseCompressionType(ct.String())
		require.NoError(t, err, "%s must round-trip through String()", ct)
		assert.Equal(t, ct, got, "%s must round-trip through String()", ct)
	}

	assert.False(t, numCompressionTypes.Valid(), "the sentinel count must not be a valid type")

	assert.Empty(t, CompressionNone.Suffix())
	assert.Equal(t, ".zstd", CompressionZstd.Suffix())
	assert.Equal(t, ".lz4", CompressionLZ4.Suffix())
}

func TestCompressConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("zero value is valid", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, CompressConfig{}.Validate())
	})

	t.Run("disabled config tolerates an unknown type", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, CompressConfig{Enabled: false, Type: "zstandard", FrameSizeKB: 7}.Validate())
	})

	t.Run("enabled config rejects an unknown type", func(t *testing.T) {
		t.Parallel()

		err := CompressConfig{Enabled: true, Type: "zstandard"}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "zstandard")
	})

	t.Run("enabled config accepts a known type", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, CompressConfig{Enabled: true, Type: " zstd "}.Validate())
	})
}

func TestCompressConfigValidateFrameSize(t *testing.T) {
	t.Parallel()

	t.Run("multiple of the block size", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, CompressConfig{Enabled: true, Type: "zstd", FrameSizeKB: 256}.ValidateFrameSize(4096))
	})

	t.Run("default frame size covers both production block sizes", func(t *testing.T) {
		t.Parallel()

		cfg := CompressConfig{Enabled: true, Type: "zstd"}
		require.NoError(t, cfg.ValidateFrameSize(4<<10), "rootfs block size")
		require.NoError(t, cfg.ValidateFrameSize(2<<20), "hugepage block size")
	})

	t.Run("frame size smaller than the block size", func(t *testing.T) {
		t.Parallel()

		err := CompressConfig{Enabled: true, Type: "zstd", FrameSizeKB: 1}.ValidateFrameSize(4096)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple")
	})

	t.Run("zero block size", func(t *testing.T) {
		t.Parallel()

		err := CompressConfig{Enabled: true, Type: "zstd"}.ValidateFrameSize(0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "block size")
	})
}
