package units

// MBShift is the bit-shift distance between bytes and megabytes.
// Use with << to convert MB to bytes, or >> to convert bytes to MB.
const MBShift = 20

// Size units and the storage stack's canonical sizes. Everything derives from
// these definitions; units_test.go fails if a derivation drifts.
const (
	KiB = 1 << 10
	MiB = 1 << MBShift
)

const (
	// PageSize is the host kernel page size (4 KiB): the UFFD page-table
	// granularity and the rootfs block size NBD serves.
	PageSize = 4 * KiB

	// HugepageSize is the x86_64 transparent-hugepage size (2 MiB): the
	// UFFD huge-page granularity and the memfile block size.
	HugepageSize = 2 * MiB

	// RootfsBlockSize is the block size of the rootfs block device.
	RootfsBlockSize = PageSize

	// MemoryChunkSize is the memory cache chunk size. It must always be
	// bigger or equal to the block size, and a whole multiple of it, so a
	// chunk always holds complete pages and frames.
	MemoryChunkSize = 4 * MiB

	// DefaultCompressFrameSize is the default uncompressed size of one
	// compression frame. It must be a multiple of every block/page size the
	// chunker can read in one block — notably HugepageSize and
	// RootfsBlockSize — or a block-sized read would span two frames.
	DefaultCompressFrameSize = 2 * MiB
)

// MBToBytes converts megabytes to bytes.
func MBToBytes(mb int64) int64 {
	return mb << MBShift
}

// BytesToMB converts bytes to megabytes.
func BytesToMB(b int64) int64 {
	return b >> MBShift
}
