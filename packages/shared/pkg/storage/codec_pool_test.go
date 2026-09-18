package storage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodecPoolBounded pins the bound: a full pool discards (and closes) extra
// instances instead of retaining them, and get returns what was pooled.
func TestCodecPoolBounded(t *testing.T) {
	t.Parallel()

	discarded := 0
	pool := newCodecPool[int](2, func(int) { discarded++ })

	pool.put(1)
	pool.put(2)
	pool.put(3)

	require.Equal(t, 2, pool.len(), "the pool retains at most its capacity")
	require.Equal(t, 1, discarded, "the overflowing instance is discarded")

	first, ok := pool.get()
	require.True(t, ok)

	second, ok := pool.get()
	require.True(t, ok)
	require.ElementsMatch(t, []int{1, 2}, []int{first, second})

	_, ok = pool.get()
	require.False(t, ok, "an empty pool reports no instance")
	require.Equal(t, 0, pool.len())
}

// TestDecoderPoolsBounded pins the shared decoder pools: they stay within their
// cap under overflow and reuse what they hold.
func TestDecoderPoolsBounded(t *testing.T) {
	t.Parallel()

	for range maxPooledCodecs + 3 {
		dec, err := getZstdDecoder(bytes.NewReader(nil))
		require.NoError(t, err)

		putZstdDecoder(dec)
	}

	require.LessOrEqual(t, zstdDecoderPool.len(), maxPooledCodecs, "zstd decoder pool must stay bounded")

	dec, err := getZstdDecoder(bytes.NewReader(nil))
	require.NoError(t, err)
	require.NotNil(t, dec)

	putZstdDecoder(dec)

	for range maxPooledCodecs + 3 {
		lz4Dec := getLZ4Decoder(bytes.NewReader(nil))
		require.NotNil(t, lz4Dec)

		putLZ4Decoder(lz4Dec)
	}

	require.LessOrEqual(t, lz4DecoderPool.len(), maxPooledCodecs, "lz4 decoder pool must stay bounded")
}

// TestCompressorPoolBoundedAndErrorTrue pins the encoder pool: a bad config
// fails at construction (the old pool.New swallowed that error and could hand
// out a compressor with a nil encoder), a good one round-trips through the
// pool, and overflow is closed rather than retained.
func TestCompressorPoolBoundedAndErrorTrue(t *testing.T) {
	t.Parallel()

	_, err := newCompressorPool(CompressConfig{Type: "not-a-compression"})
	require.Error(t, err, "an unsupported compression type must fail at construction")

	pool, err := newCompressorPool(CompressConfig{Type: "lz4"})
	require.NoError(t, err)

	first, err := pool.get()
	require.NoError(t, err)

	out, err := first.compress([]byte("payload"))
	require.NoError(t, err)
	require.NotEmpty(t, out)

	pool.put(first)

	again, err := pool.get()
	require.NoError(t, err)
	require.Same(t, first, again, "the compressor is reused from the pool")

	for range maxPooledCodecs + 3 {
		c, err := pool.get()
		require.NoError(t, err)

		pool.put(c)
	}

	require.LessOrEqual(t, pool.pool.len(), maxPooledCodecs, "compressor pool must stay bounded")
}
