//go:build linux

package ublk

import (
	"testing"
)

// The sizes and offsets here come from include/uapi/linux/ublk_cmd.h. The
// kernel reads these structs straight out of the SQE, so a mismatch is not a
// compile error, it is a device that misbehaves.
func TestABILayout(t *testing.T) {
	t.Parallel()

	t.Run("sizes", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name string
			got  int
			want int
		}{
			{"ctrl_cmd", ctrlCmdSize, 32},
			{"ctrl_dev_info", ctrlDevInfoSize, 64},
			{"io_cmd", ioCmdSize, 16},
			{"io_desc", ioDescSize, 24},
		} {
			if tc.got != tc.want {
				t.Errorf("%s: size %d, want %d", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("ctrl_cmd offsets", func(t *testing.T) {
		t.Parallel()

		// dev_id, queue_id, len, addr, data[1]
		if ctrlCmdOffDevID != 0 || ctrlCmdOffQueueID != 4 || ctrlCmdOffLength != 6 ||
			ctrlCmdOffAddr != 8 || ctrlCmdOffData != 16 {
			t.Errorf("ctrl_cmd offsets changed: %d %d %d %d %d",
				ctrlCmdOffDevID, ctrlCmdOffQueueID, ctrlCmdOffLength, ctrlCmdOffAddr, ctrlCmdOffData)
		}
	})

	t.Run("ctrl_dev_info offsets", func(t *testing.T) {
		t.Parallel()

		if ctrlDevInfoOffNrHWQueues != 0 || ctrlDevInfoOffQueueDepth != 2 ||
			ctrlDevInfoOffIODescSize != 6 || ctrlDevInfoOffMaxIOBufBytes != 8 ||
			ctrlDevInfoOffDevID != 12 || ctrlDevInfoOffFlags != 24 {
			t.Error("ctrl_dev_info offsets changed")
		}
	})

	t.Run("io_cmd offsets", func(t *testing.T) {
		t.Parallel()

		if ioCmdOffQID != 0 || ioCmdOffTag != 2 || ioCmdOffResult != 4 || ioCmdOffAddr != 8 {
			t.Error("io_cmd offsets changed")
		}
	})

	t.Run("params prefix", func(t *testing.T) {
		t.Parallel()

		// len and types, then ublk_param_basic (32 bytes) and
		// ublk_param_discard (20 bytes).
		if paramsBasicOffset != 8 || paramsBasicOffset+32 != paramsDiscardOffset || paramsDiscardOffset+20 != paramsLen {
			t.Errorf("params layout changed: basic %d, discard %d, len %d",
				paramsBasicOffset, paramsDiscardOffset, paramsLen)
		}
	})
}

// The plain command numbers, and the ioctl-encoded form the driver expects
// when UBLK_F_CMD_IOCTL_ENCODE is set. _IOWR('u', nr, size) is dir 3 in the
// top bits, then size, type and number.
func TestCommandEncoding(t *testing.T) {
	t.Parallel()

	const wantDir = 3

	for _, tc := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"add_dev", controlOp(true, ublkCmdAddDev), wantDir<<30 | ctrlCmdSize<<16 | 'u'<<8 | ublkCmdAddDev},
		{"set_params", controlOp(true, ublkCmdSetParams), wantDir<<30 | ctrlCmdSize<<16 | 'u'<<8 | ublkCmdSetParams},
		{"start_dev", controlOp(true, ublkCmdStartDev), wantDir<<30 | ctrlCmdSize<<16 | 'u'<<8 | ublkCmdStartDev},
		{"stop_dev", controlOp(true, ublkCmdStopDev), wantDir<<30 | ctrlCmdSize<<16 | 'u'<<8 | ublkCmdStopDev},
		{"del_dev", controlOp(true, ublkCmdDelDev), wantDir<<30 | ctrlCmdSize<<16 | 'u'<<8 | ublkCmdDelDev},
		{"fetch_req", dataOp(true, ublkIOFetchReq), wantDir<<30 | ioCmdSize<<16 | 'u'<<8 | ublkIOFetchReq},
		{"commit_and_fetch", dataOp(true, ublkIOCommitAndFetch), wantDir<<30 | ioCmdSize<<16 | 'u'<<8 | ublkIOCommitAndFetch},
		{"legacy_add_dev", controlOp(false, ublkCmdAddDev), ublkCmdAddDev},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: cmd_op %#x, want %#x", tc.name, tc.got, tc.want)
		}
	}
}

// ioPos is ublk_pos(): the kernel decodes queue, tag and offset from the
// position a user-copy pread or pwrite is made at.
func TestIOPos(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		qid, tag uint16
		off      uint32
	}{
		{0, 0, 0},
		{0, 0, 4096},
		{1, 2, 0},
		{3, 63, 1 << 20},
		{4095, 4095, ublkIOBufOffset - 1},
	} {
		got := ioPos(tc.qid, tc.tag, tc.off)
		want := int64(ublkIOBufOffset) | int64(tc.qid)<<ublkQIDOff | int64(tc.tag)<<ublkTagOff | int64(tc.off)
		if got != want {
			t.Errorf("ioPos(%d, %d, %d) = %#x, want %#x", tc.qid, tc.tag, tc.off, got, want)
		}
		if got&int64(ublkIOBufOffset) == 0 {
			t.Errorf("ioPos(%d, %d, %d) = %#x does not carry the user-copy flag", tc.qid, tc.tag, tc.off, got)
		}
	}
}

func TestUserData(t *testing.T) {
	t.Parallel()

	if got, want := userData(7, ublkIOCommitAndFetch), uint64(7)|uint64(0x21)<<16; got != want {
		t.Errorf("userData = %#x, want %#x", got, want)
	}

	// The tag has to survive the pack, because that is all the data plane
	// reads back out of a completion.
	ud := userData(0xbeef, ublkIOFetchReq)
	if got := uint16(ud); got != 0xbeef {
		t.Errorf("tag round-trip = %#x, want %#x", got, 0xbeef)
	}
}

func TestBuildParams(t *testing.T) {
	t.Parallel()

	t.Run("with discard", func(t *testing.T) {
		t.Parallel()

		params := buildParams(1<<21, 8192, 4096, true)

		if len(params) != paramsLen {
			t.Fatalf("len = %d, want %d", len(params), paramsLen)
		}
		if got := getU32(params, 0); got != paramsLen {
			t.Errorf("params.len = %d, want %d", got, paramsLen)
		}
		if got, want := getU32(params, 4), uint32(ublkParamTypeBasic|ublkParamTypeDiscard); got != want {
			t.Errorf("params.types = %#x, want %#x", got, want)
		}

		if got := getU32(params, paramsBasicOffset); got != 0 {
			t.Errorf("attrs = %#x, want 0 (no write cache, no FUA)", got)
		}
		for _, off := range []int{4, 5, 6, 7} {
			if got := params[paramsBasicOffset+off]; got != 12 {
				t.Errorf("basic shift at +%d = %d, want 12", off, got)
			}
		}
		if got := getU32(params, paramsBasicOffset+8); got != 8192 {
			t.Errorf("max_sectors = %d, want 8192", got)
		}
		if got := getU64(params, paramsBasicOffset+16); got != 1<<21 {
			t.Errorf("dev_sectors = %d, want %d", got, 1<<21)
		}

		if got := getU32(params, paramsDiscardOffset+4); got != 4096 {
			t.Errorf("discard_granularity = %d, want 4096", got)
		}
		if got := getU32(params, paramsDiscardOffset+8); got != maxDiscardSectors {
			t.Errorf("max_discard_sectors = %d, want %d", got, maxDiscardSectors)
		}
		if got := getU32(params, paramsDiscardOffset+12); got != maxDiscardSectors {
			t.Errorf("max_write_zeroes_sectors = %d, want %d", got, maxDiscardSectors)
		}
		// The kernel rejects a discard type whose granularity is zero or
		// whose max_discard_segments is not 1.
		if got := getU16(params, paramsDiscardOffset+16); got != 1 {
			t.Errorf("max_discard_segments = %d, want 1", got)
		}
	})

	t.Run("without discard", func(t *testing.T) {
		t.Parallel()

		params := buildParams(1<<21, 8192, 512, false)

		if len(params) != paramsDiscardOffset {
			t.Fatalf("len = %d, want %d", len(params), paramsDiscardOffset)
		}
		if got, want := getU32(params, 4), uint32(ublkParamTypeBasic); got != want {
			t.Errorf("params.types = %#x, want %#x", got, want)
		}
		if got := params[paramsBasicOffset+4]; got != 9 {
			t.Errorf("logical_bs_shift = %d, want 9", got)
		}
	})
}

func TestHelpers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in, want uint32
	}{
		{0, 2},
		{1, 2},
		{2, 2},
		{3, 4},
		{64, 64},
		{65, 128},
		{4096, 4096},
	} {
		if got := nextPowerOfTwo(tc.in); got != tc.want {
			t.Errorf("nextPowerOfTwo(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		v, align, want int
	}{
		{0, 4096, 0},
		{1, 4096, 4096},
		{4096, 4096, 4096},
		{4097, 4096, 8192},
		{98304, 4096, 98304},
	} {
		if got := roundUp(tc.v, tc.align); got != tc.want {
			t.Errorf("roundUp(%d, %d) = %d, want %d", tc.v, tc.align, got, tc.want)
		}
	}
}
