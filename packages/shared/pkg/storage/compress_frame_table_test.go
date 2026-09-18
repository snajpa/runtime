package storage

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// threeFrameFT returns a FrameTable with three 1MB uncompressed frames
// and varying compressed sizes, starting at the given offset.
func threeFrameFT(startU, startC int64) *FrameTable {
	return &FrameTable{
		compressionType: CompressionLZ4,
		entries: []frameEntry{
			{StartU: startU, StartC: startC, SizeU: 1 << 20, SizeC: 500_000},                     // frame 0
			{StartU: startU + 1<<20, StartC: startC + 500_000, SizeU: 1 << 20, SizeC: 600_000},   // frame 1
			{StartU: startU + 2<<20, StartC: startC + 1_100_000, SizeU: 1 << 20, SizeC: 400_000}, // frame 2
		},
	}
}

func TestLocate(t *testing.T) {
	t.Parallel()
	ft := threeFrameFT(0, 0)

	t.Run("first byte of each frame uncompressed", func(t *testing.T) {
		t.Parallel()
		for i, wantU := range []int64{0, 1 << 20, 2 << 20} {
			r, err := ft.LocateUncompressed(wantU)
			require.NoError(t, err, "frame %d", i)
			require.Equal(t, wantU, r.Offset)
			require.Equal(t, 1<<20, r.Length)
		}
	})

	t.Run("first byte of each frame compressed", func(t *testing.T) {
		t.Parallel()
		wantC := []int64{0, 500_000, 1_100_000}
		wantLen := []int{500_000, 600_000, 400_000}
		for i, offsetU := range []int64{0, 1 << 20, 2 << 20} {
			r, err := ft.LocateCompressed(offsetU)
			require.NoError(t, err, "frame %d", i)
			require.Equal(t, wantC[i], r.Offset, "frame %d C start", i)
			require.Equal(t, wantLen[i], r.Length, "frame %d C length", i)
		}
	})

	t.Run("last byte of frame", func(t *testing.T) {
		t.Parallel()
		r, err := ft.LocateUncompressed((1 << 20) - 1)
		require.NoError(t, err)
		require.Equal(t, int64(0), r.Offset)
	})

	t.Run("returns correct C offset", func(t *testing.T) {
		t.Parallel()
		r, err := ft.LocateCompressed(2 << 20)
		require.NoError(t, err)
		require.Equal(t, int64(1_100_000), r.Offset) // 500k + 600k
	})

	t.Run("beyond end errors", func(t *testing.T) {
		t.Parallel()
		_, err := ft.LocateCompressed(3 << 20)
		require.Error(t, err)
	})

	t.Run("mid-frame offset errors", func(t *testing.T) {
		t.Parallel()
		// A mid-frame uncompressed offset would fetch and decode the whole
		// containing frame from its start, silently returning data for the
		// wrong position. It must be rejected so callers frame-align first.
		_, err := ft.LocateCompressed((1 << 20) / 2)
		require.Error(t, err)
		require.Contains(t, err.Error(), "frame-aligned")
	})

	t.Run("nil table errors", func(t *testing.T) {
		t.Parallel()
		_, err := (*FrameTable)(nil).LocateCompressed(0)
		require.Error(t, err)
	})

	t.Run("non-zero start offset", func(t *testing.T) {
		t.Parallel()
		sub := threeFrameFT(1<<20, 500_000)

		r, err := sub.LocateUncompressed(1 << 20)
		require.NoError(t, err)
		require.Equal(t, int64(1<<20), r.Offset)

		r, err = sub.LocateCompressed(1 << 20)
		require.NoError(t, err)
		require.Equal(t, int64(500_000), r.Offset)

		// Before first entry — no frame should contain offset 0.
		_, err = sub.LocateUncompressed(0)
		require.Error(t, err)
	})
}

func TestNewFrameTable(t *testing.T) {
	t.Parallel()

	ft := NewFullFrameTable(CompressionZstd, []FrameSize{
		{U: 1 << 20, C: 500_000},
		{U: 1 << 20, C: 600_000},
	}).Table()

	require.Equal(t, 2, ft.NumFrames())
	require.Equal(t, CompressionZstd, ft.CompressionType())
	require.True(t, ft.IsCompressed())
	require.Equal(t, int64(2<<20), ft.UncompressedSize())
	require.Equal(t, int64(1_100_000), ft.CompressedSize())

	startU, endU, startC, endC := ft.FrameAt(0)
	require.Equal(t, int64(0), startU)
	require.Equal(t, int64(1<<20), endU)
	require.Equal(t, int64(0), startC)
	require.Equal(t, int64(500_000), endC)

	startU, _, startC, _ = ft.FrameAt(1)
	require.Equal(t, int64(1<<20), startU)
	require.Equal(t, int64(500_000), startC)
}

func TestFrameTable_TrimToRanges(t *testing.T) {
	t.Parallel()

	ft := NewFullFrameTable(CompressionLZ4, []FrameSize{
		{U: 1 << 20, C: 500_000},
		{U: 1 << 20, C: 600_000},
		{U: 1 << 20, C: 400_000},
		{U: 1 << 20, C: 700_000},
	}).Table()

	t.Run("all frames retained", func(t *testing.T) {
		t.Parallel()
		trimmed := ft.TrimToRanges([]Range{{Offset: 0, Length: 4 << 20}})
		require.Equal(t, ft.NumFrames(), trimmed.NumFrames())
	})

	t.Run("single range trims to subset", func(t *testing.T) {
		t.Parallel()
		trimmed := ft.TrimToRanges([]Range{{Offset: 1 << 20, Length: 2 << 20}})
		require.Equal(t, 2, trimmed.NumFrames())

		startU, _, _, _ := trimmed.FrameAt(0)
		require.Equal(t, int64(1<<20), startU)

		startU, _, _, _ = trimmed.FrameAt(1)
		require.Equal(t, int64(2<<20), startU)
	})

	t.Run("two disjoint ranges", func(t *testing.T) {
		t.Parallel()
		trimmed := ft.TrimToRanges([]Range{
			{Offset: 0, Length: 1 << 20},
			{Offset: 3 << 20, Length: 1 << 20},
		})
		require.Equal(t, 2, trimmed.NumFrames())

		startU, _, _, _ := trimmed.FrameAt(0)
		require.Equal(t, int64(0), startU)

		startU, _, _, _ = trimmed.FrameAt(1)
		require.Equal(t, int64(3<<20), startU)
	})

	t.Run("nil table", func(t *testing.T) {
		t.Parallel()
		var nilFT *FrameTable
		require.Nil(t, nilFT.TrimToRanges([]Range{{Offset: 0, Length: 100}}))
	})

	t.Run("sparse lookup works", func(t *testing.T) {
		t.Parallel()
		trimmed := ft.TrimToRanges([]Range{
			{Offset: 0, Length: 1 << 20},
			{Offset: 3 << 20, Length: 1 << 20},
		})

		r, err := trimmed.LocateCompressed(0)
		require.NoError(t, err)
		require.Equal(t, int64(0), r.Offset)

		r, err = trimmed.LocateCompressed(3 << 20)
		require.NoError(t, err)
		require.Equal(t, int64(500_000+600_000+400_000), r.Offset)

		// Gap lookup fails
		_, err = trimmed.LocateCompressed(1 << 20)
		require.Error(t, err)
	})
}

func TestSerializeDeserializeFrameTable(t *testing.T) {
	t.Parallel()

	t.Run("round-trip", func(t *testing.T) {
		t.Parallel()
		ft := NewFullFrameTable(CompressionZstd, []FrameSize{
			{U: 2048, C: 1024},
			{U: 4096, C: 3500},
		}).Table()

		var buf bytes.Buffer
		require.NoError(t, ft.Serialize(&buf))

		got, err := DeserializeFrameTable(&buf)
		require.NoError(t, err)
		require.Equal(t, ft.NumFrames(), got.NumFrames())
		require.Equal(t, ft.CompressionType(), got.CompressionType())

		for i := range ft.NumFrames() {
			wSU, wEU, wSC, wEC := ft.FrameAt(i)
			gSU, gEU, gSC, gEC := got.FrameAt(i)
			require.Equal(t, wSU, gSU, "frame %d StartU", i)
			require.Equal(t, wEU, gEU, "frame %d EndU", i)
			require.Equal(t, wSC, gSC, "frame %d StartC", i)
			require.Equal(t, wEC, gEC, "frame %d EndC", i)
		}
	})

	t.Run("nil writes zeros", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		require.NoError(t, (*FrameTable)(nil).Serialize(&buf))

		got, err := DeserializeFrameTable(&buf)
		require.NoError(t, err)
		require.Nil(t, got)
	})
}

// TestDeserializeFrameTableRejectsUnknownCompressionType pins the guard for
// headers whose compression-type word narrows to CompressionNone while the
// frame count is non-zero. Accepting one used to build a table that owns
// frames yet reports "no compression"; Serialize then wrote ct=0/n=0 for it
// and the re-parse returned nil — the round trip lost every frame. Regression
// for testdata/fuzz/FuzzDeserializeFrameTable/a8db7817e8b06081.
func TestDeserializeFrameTableRejectsUnknownCompressionType(t *testing.T) {
	t.Parallel()

	// Header: LE uint32 0x30303000 (low byte = none) + frame count 1 + one
	// 24-byte entry — the exact shape the fuzzer found.
	buf := append(
		[]byte{0x00, 0x30, 0x30, 0x30, 0x01, 0x00, 0x00, 0x00},
		bytes.Repeat([]byte{0x30}, 24)...,
	)

	_, err := DeserializeFrameTable(bytes.NewReader(buf))
	require.Error(t, err, "a narrowing compression type must be rejected")
	require.Contains(t, err.Error(), "unknown compression type")

	// A genuinely unknown codec is rejected the same way.
	buf[0] = 0x7f
	_, err = DeserializeFrameTable(bytes.NewReader(buf))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown compression type")
}

// TestDeserializeFrameTableRejectsInvalidFrameOffsets pins the boundary
// checks for frame starts: StartU/StartC are absolute stream offsets and
// must be non-negative, and Start+Size must not wrap int64. Both were
// previously unvalidated for the first entry (the ordering checks ran only
// for i > 0), so a crafted table with StartU=-1, StartC=-1, SizeU=1, SizeC=1
// parsed cleanly and FrameAt(0) returned negative ranges (reviewer1's probe).
func TestDeserializeFrameTableRejectsInvalidFrameOffsets(t *testing.T) {
	t.Parallel()

	const maxInt64 int64 = 1<<63 - 1

	entry := func(startU, startC int64, sizeU, sizeC int32) []byte {
		var b bytes.Buffer
		require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(CompressionLZ4)))
		require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(1)))
		require.NoError(t, binary.Write(&b, binary.LittleEndian, startU))
		require.NoError(t, binary.Write(&b, binary.LittleEndian, startC))
		require.NoError(t, binary.Write(&b, binary.LittleEndian, sizeU))
		require.NoError(t, binary.Write(&b, binary.LittleEndian, sizeC))

		return b.Bytes()
	}

	t.Run("negative starts (reviewer case)", func(t *testing.T) {
		t.Parallel()

		_, err := DeserializeFrameTable(bytes.NewReader(entry(-1, -1, 1, 1)))
		require.Error(t, err, "negative StartU/StartC must be rejected")
		require.Contains(t, err.Error(), "negative start")
	})

	t.Run("negative StartC only", func(t *testing.T) {
		t.Parallel()

		_, err := DeserializeFrameTable(bytes.NewReader(entry(0, -1, 1, 1)))
		require.Error(t, err)
		require.Contains(t, err.Error(), "negative start")
	})

	t.Run("StartU end overflow", func(t *testing.T) {
		t.Parallel()

		_, err := DeserializeFrameTable(bytes.NewReader(entry(maxInt64-1, 0, 2, 1)))
		require.Error(t, err, "StartU+SizeU must not wrap")
		require.Contains(t, err.Error(), "overflows")
	})

	t.Run("StartC end overflow", func(t *testing.T) {
		t.Parallel()

		_, err := DeserializeFrameTable(bytes.NewReader(entry(0, maxInt64-1, 1, 2)))
		require.Error(t, err)
		require.Contains(t, err.Error(), "overflows")
	})

	t.Run("valid single frame still parses", func(t *testing.T) {
		t.Parallel()

		ft, err := DeserializeFrameTable(bytes.NewReader(entry(0, 0, 1, 1)))
		require.NoError(t, err)
		require.NotNil(t, ft)
		startU, endU, startC, endC := ft.FrameAt(0)
		require.Equal(t, int64(0), startU)
		require.Equal(t, int64(1), endU)
		require.Equal(t, int64(0), startC)
		require.Equal(t, int64(1), endC)
	})
}
