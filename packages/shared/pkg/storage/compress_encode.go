package storage

import (
	"bytes"
	"context"
	"fmt"

	"github.com/klauspost/compress/zstd"
	lz4 "github.com/pierrec/lz4/v4"
)

// compressor compresses individual frames. Implementations are pooled and
// reused across frames within a single CompressStream call.
type compressor interface {
	compress(src []byte) ([]byte, error)
}

// lz4Compressor wraps a pooled lz4.Writer. The writer is reused via Reset
// between frames to avoid re-allocating internal hash tables (~64KB).
type lz4Compressor struct {
	w *lz4.Writer
}

func (c *lz4Compressor) compress(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(lz4.CompressBlockBound(len(src)))
	c.w.Reset(&buf)

	if _, err := c.w.Write(src); err != nil {
		return nil, fmt.Errorf("lz4 compress: %w", err)
	}

	if err := c.w.Close(); err != nil {
		return nil, fmt.Errorf("lz4 compress close: %w", err)
	}

	return buf.Bytes(), nil
}

// zstdCompressor wraps a pooled zstd.Encoder using EncodeAll.
type zstdCompressor struct {
	enc *zstd.Encoder
}

func (z *zstdCompressor) compress(src []byte) ([]byte, error) { //nolint:unparam // satisfies compressor interface
	return z.enc.EncodeAll(src, make([]byte, 0, len(src))), nil
}

// compressorPool is a bounded pool of compressors plus the options used to
// construct new ones (see codecPool for the bound). Construction errors are
// returned to the caller: the old sync.Pool.New swallowed zstd.NewWriter's
// error and handed out a compressor with a nil encoder, whose first EncodeAll
// would panic.
type compressorPool struct {
	pool     *codecPool[compressor]
	ct       CompressionType
	zstdOpts []zstd.EOption
	lz4Opts  []lz4.Option
}

// newCompressorPool validates cfg by building one compressor eagerly and
// pooling it, so a bad config fails here rather than at the first frame.
func newCompressorPool(cfg CompressConfig) (*compressorPool, error) {
	p := &compressorPool{
		pool: newCodecPool[compressor](maxPooledCodecs, discardCompressor),
		ct:   cfg.CompressionType(),
	}

	switch p.ct {
	case CompressionZstd:
		p.zstdOpts = []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevel(cfg.Level)),
			zstd.WithEncoderCRC(true),
		}
		if cfg.FrameSize() > 0 {
			p.zstdOpts = append(p.zstdOpts, zstd.WithWindowSize(cfg.FrameSize()))
		}
		if cfg.EncoderConcurrency > 0 {
			p.zstdOpts = append(p.zstdOpts, zstd.WithEncoderConcurrency(cfg.EncoderConcurrency))
		}
	case CompressionLZ4:
		p.lz4Opts = []lz4.Option{
			lz4.BlockSizeOption(lz4.Block4Mb),
			lz4.BlockChecksumOption(true),
			lz4.ChecksumOption(false),
			lz4.ConcurrencyOption(1),
			lz4.CompressionLevelOption(lz4.Fast),
		}
	default:
		return nil, fmt.Errorf("unsupported compression type: %s", cfg.CompressionType())
	}

	first, err := p.newCompressor()
	if err != nil {
		return nil, err
	}
	p.pool.put(first)

	return p, nil
}

// get returns a pooled compressor, constructing one when the pool is empty.
func (p *compressorPool) get() (compressor, error) {
	if c, ok := p.pool.get(); ok {
		return c, nil
	}

	return p.newCompressor()
}

// put returns a compressor for reuse; a full pool closes it instead of
// retaining it.
func (p *compressorPool) put(c compressor) {
	p.pool.put(c)
}

func (p *compressorPool) newCompressor() (compressor, error) {
	switch p.ct {
	case CompressionZstd:
		enc, err := zstd.NewWriter(nil, p.zstdOpts...)
		if err != nil {
			return nil, fmt.Errorf("zstd encoder: %w", err)
		}

		return &zstdCompressor{enc: enc}, nil
	case CompressionLZ4:
		w := lz4.NewWriter(nil)
		if err := w.Apply(p.lz4Opts...); err != nil {
			return nil, fmt.Errorf("lz4 encoder: %w", err)
		}

		return &lz4Compressor{w: w}, nil
	default:
		return nil, fmt.Errorf("unsupported compression type: %s", p.ct)
	}
}

func discardCompressor(c compressor) {
	switch v := c.(type) {
	case *zstdCompressor:
		v.enc.Close()
	case *lz4Compressor:
		_ = v.w.Close()
	}
}

func CompressBytes(ctx context.Context, data []byte, cfg CompressConfig) (*FullFrameTable, []byte, [32]byte, error) {
	up := &memPartUploader{}

	const compressBytesConcurrency = 1
	ft, checksum, err := compressStream(ctx, bytes.NewReader(data), cfg, up, compressBytesConcurrency, nil)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}

	return ft, up.Assemble(), checksum, nil
}
