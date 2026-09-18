package storage

import (
	"errors"
	"fmt"

	"github.com/e2b-dev/infra/packages/shared/pkg/units"
)

const (
	// DefaultCompressFrameSize is the default uncompressed size of each
	// compression frame (2 MiB). Overridable via CompressConfig.FrameSizeKB.
	// The last frame in a file may be shorter.
	//
	// The chunker fetches one frame at a time from storage on a cache miss.
	// Larger frame sizes mean more data cached per fetch (faster warm-up and
	// fewer remote storage round-trips), but higher memory and I/O cost per
	// miss.
	//
	// This MUST be multiple of every block/page size:
	//   - header.HugepageSize (2 MiB) — UFFD huge-page size, also used by prefetch
	//   - header.RootfsBlockSize (4 KiB) — NBD / rootfs block size
	DefaultCompressFrameSize = units.DefaultCompressFrameSize

	// Use case identifiers for per-use-case compression targeting via LaunchDarkly.
	UseCaseBuild = "build"
	UseCasePause = "pause"
)

// CompressConfig is the base compression configuration, loaded from environment
// variables at startup. Feature flags may override individual fields at runtime
// at the upload boundary. Zero value means compression disabled.
type CompressConfig struct {
	Enabled            bool   `env:"COMPRESS_ENABLED"              envDefault:"false"`
	Type               string `env:"COMPRESS_TYPE"                 envDefault:""`
	Level              int    `env:"COMPRESS_LEVEL"                envDefault:"0"`
	FrameSizeKB        int    `env:"COMPRESS_FRAME_SIZE_KB"        envDefault:"0"`
	MinPartSizeMB      int    `env:"COMPRESS_MIN_PART_SIZE_MB"     envDefault:"0"`
	FrameEncodeWorkers int    `env:"COMPRESS_FRAME_ENCODE_WORKERS" envDefault:"0"`
	EncoderConcurrency int    `env:"COMPRESS_ENCODER_CONCURRENCY"  envDefault:"0"`
}

// CompressionType returns the parsed CompressionType.
func (c CompressConfig) CompressionType() CompressionType {
	return parseCompressionType(c.Type)
}

// FrameSize returns the frame size in bytes.
func (c CompressConfig) FrameSize() int {
	if c.FrameSizeKB <= 0 {
		return DefaultCompressFrameSize
	}

	return c.FrameSizeKB * 1024
}

// MinPartSize returns the minimum compressed part size in bytes.
// Parts accumulate frames until they reach this threshold.
func (c CompressConfig) MinPartSize() int64 {
	if c.MinPartSizeMB <= 0 {
		return int64(gcpMultipartUploadChunkSize)
	}

	return int64(c.MinPartSizeMB) * (1 << 20)
}

// IsCompressionEnabled reports whether compression is configured and active.
func (c CompressConfig) IsCompressionEnabled() bool {
	return c.Enabled && c.CompressionType() != CompressionNone
}

// Validate checks the configuration for internal consistency. A disabled
// configuration is always valid because none of its fields are used; an
// enabled one must name a supported compression type. Frame-size checks that
// depend on the reader's block size are in ValidateFrameSize.
func (c CompressConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	if _, err := ParseCompressionType(c.Type); err != nil {
		return err
	}

	if fs := c.FrameSize(); fs <= 0 {
		return fmt.Errorf("frame size must be positive, got %d KB", c.FrameSizeKB)
	}

	return nil
}

// ValidateFrameSize checks that the frame size is a positive multiple of
// blockSize so that every block-sized read served by the chunker lies inside
// one frame — otherwise Chunker.fetch fetches only the start frame and
// cache.sliceDirect returns uninitialized mmap bytes for the tail.
func (c CompressConfig) ValidateFrameSize(blockSize uint64) error {
	fs := c.FrameSize()
	if fs <= 0 {
		return fmt.Errorf("frame size must be positive, got %d KB", c.FrameSizeKB)
	}

	if blockSize == 0 {
		return errors.New("block size must be positive")
	}

	if uint64(fs)%blockSize != 0 {
		return fmt.Errorf("frame size (%d) must be a multiple of block size (%d)", fs, blockSize)
	}

	return nil
}
