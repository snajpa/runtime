package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPayloadDataIsDeterministicAndSized(t *testing.T) {
	t.Parallel()

	a := payloadData(7, 4096)
	b := payloadData(7, 4096)
	c := payloadData(8, 4096)

	if len(a) != 4096 {
		t.Fatalf("length = %d, want 4096", len(a))
	}

	if string(a) != string(b) {
		t.Fatal("same seed produced different payloads")
	}

	if string(a) == string(c) {
		t.Fatal("different seeds produced identical payloads")
	}

	// The generator is meant to look like real data: some compressible runs,
	// some incompressible bytes. A payload of one repeated byte would make the
	// frame tables and the compressed sizes meaningless.
	distinct := map[byte]struct{}{}
	for _, v := range a {
		distinct[v] = struct{}{}
	}

	if len(distinct) < 16 {
		t.Fatalf("payload has only %d distinct byte values", len(distinct))
	}
}

func TestBuildIDDependsOnPrefixAndIndex(t *testing.T) {
	t.Parallel()

	first := buildID("run-1", 0)
	again := buildID("run-1", 0)
	second := buildID("run-1", 1)
	other := buildID("run-2", 0)

	if first != again {
		t.Fatal("build ID is not deterministic")
	}

	if first == second || first == other {
		t.Fatal("build IDs collide across index or prefix")
	}
}

func TestPickProfile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		builds  int
		payload int64
		want    profile
	}{
		{"tiny", 0, 0, profile{builds: 2, payload: 4 << 20}},
		{"small", 0, 0, profile{builds: 4, payload: 16 << 20}},
		{"big", 0, 0, profile{builds: 16, payload: 256 << 20}},
		// Explicit flags win over the profile.
		{"tiny", 9, 0, profile{builds: 9, payload: 4 << 20}},
		{"big", 0, 1024, profile{builds: 16, payload: 1024}},
	}

	for _, tc := range cases {
		got := pickProfile(tc.name, tc.builds, tc.payload)
		if got != tc.want {
			t.Errorf("pickProfile(%q, %d, %d) = %+v, want %+v", tc.name, tc.builds, tc.payload, got, tc.want)
		}
	}
}

func TestPickProfileAutoScalesToTheBox(t *testing.T) {
	t.Parallel()

	got := pickProfile("auto", 0, 0)

	if got.builds < 2 || got.builds > 32 {
		t.Errorf("auto builds = %d, want 2..32", got.builds)
	}

	if got.payload < 4<<20 || got.payload > 256<<20 {
		t.Errorf("auto payload = %d, want 4MiB..256MiB", got.payload)
	}
}

func TestCompressConfigUsesProductionFrameShape(t *testing.T) {
	t.Parallel()

	cfg := compressConfig()

	if !cfg.Enabled {
		t.Error("compression is disabled")
	}

	if cfg.Type != codecZstd {
		t.Errorf("codec = %q, want %q", cfg.Type, codecZstd)
	}

	if cfg.FrameSizeKB != frameSizeKB {
		t.Errorf("frame size = %d KiB, want %d", cfg.FrameSizeKB, frameSizeKB)
	}

	if cfg.MinPartSizeMB != minPartSizeMB {
		t.Errorf("min part size = %d MiB, want %d", cfg.MinPartSizeMB, minPartSizeMB)
	}

	if cfg.FrameEncodeWorkers < 1 {
		t.Errorf("frame encode workers = %d, want >= 1", cfg.FrameEncodeWorkers)
	}
}

func TestLoadManifestRoundTripsAndRejectsBadInput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	want := manifest{
		Version:    "test@deadbeef",
		StorageURL: "s3://bucket?endpoint=http://127.0.0.1:9000",
		Prefix:     "run-1",
		Entries: []entry{{
			Build:     "11111111-1111-1111-1111-111111111111",
			Payload:   "run-1/builds/x/rootfs.ext4",
			Header:    "run-1/builds/x/rootfs.ext4.header",
			Size:      7 << 20,
			Checksum:  "aa",
			Codec:     codecZstd,
			WrittenBy: "test@deadbeef",
		}},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}

	if got.Prefix != want.Prefix || len(got.Entries) != 1 || got.Entries[0].Checksum != want.Entries[0].Checksum {
		t.Fatalf("round trip changed the manifest: %+v", got)
	}

	if _, err := loadManifest(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing manifest did not fail")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	if _, err := loadManifest(bad); err == nil {
		t.Error("invalid manifest did not fail")
	}
}

func TestBytesBufferAccumulates(t *testing.T) {
	t.Parallel()

	var buf bytesBuffer

	if _, err := buf.Write([]byte("abc")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := buf.Write([]byte("de")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := string(buf.Bytes()); got != "abcde" {
		t.Fatalf("buffer = %q, want %q", got, "abcde")
	}
}
