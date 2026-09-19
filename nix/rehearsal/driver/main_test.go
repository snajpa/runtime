package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
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

func TestSprayProfile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		count     int
		size      int64
		wantCount int
		wantSize  int64
	}{
		{"tiny", 0, 0, 500, 4 << 10},
		{"small", 0, 0, 2000, 4 << 10},
		{"big", 0, 0, 20000, 4 << 10},
		// explicit overrides win, whatever the profile says
		{"tiny", 7, 1 << 20, 7, 1 << 20},
	}

	for _, tc := range cases {
		gotCount, gotSize := sprayProfile(tc.name, tc.count, tc.size)
		if gotCount != tc.wantCount || gotSize != tc.wantSize {
			t.Errorf("sprayProfile(%q, %d, %d) = (%d, %d), want (%d, %d)",
				tc.name, tc.count, tc.size, gotCount, gotSize, tc.wantCount, tc.wantSize)
		}
	}

	// auto must stay inside the documented bounds on any machine
	gotCount, gotSize := sprayProfile("auto", 0, 0)
	if gotCount < 500 || gotCount > 20000 {
		t.Errorf("auto count = %d, want 500..20000", gotCount)
	}

	if gotSize != 4<<10 {
		t.Errorf("auto size = %d, want 4096", gotSize)
	}
}

func TestRetryBackoffStaysBounded(t *testing.T) {
	t.Parallel()

	for attempt := range 5 {
		got := retryBackoff(attempt)
		if got < 0 || got > 2*time.Second {
			t.Fatalf("retryBackoff(%d) = %v, want 0..2s", attempt, got)
		}
	}
}

func TestRetrySummary(t *testing.T) {
	t.Parallel()

	if got := retrySummary(0, 100); got != "no retries" {
		t.Errorf("retrySummary(0, 100) = %q", got)
	}

	if got := retrySummary(5, 100); got != "5 retries (5.00% of writes)" {
		t.Errorf("retrySummary(5, 100) = %q", got)
	}
}

// flakyBlob fails the first fails-before-success writes, then succeeds.
type flakyBlob struct {
	fails int
	puts  int
}

func (b *flakyBlob) WriteTo(context.Context, io.Writer) (int64, error) { return 0, nil }
func (b *flakyBlob) Exists(context.Context) (bool, error)              { return true, nil }

func (b *flakyBlob) Put(_ context.Context, _ []byte, _ ...storage.PutOption) error {
	b.puts++
	if b.puts <= b.fails {
		return context.DeadlineExceeded
	}

	return nil
}

func TestPutWithRetry(t *testing.T) {
	t.Parallel()

	// succeeds on the second attempt
	blob := &flakyBlob{fails: 1}
	attempts, err := putWithRetry(t.Context(), blob, []byte("x"), 3)
	if err != nil || attempts != 2 {
		t.Fatalf("putWithRetry = (%d, %v), want (2, nil)", attempts, err)
	}

	// gives up after the configured attempts
	blob = &flakyBlob{fails: 99}
	attempts, err = putWithRetry(t.Context(), blob, []byte("x"), 2)
	if err == nil || attempts != 2 {
		t.Fatalf("putWithRetry = (%d, %v), want (2, error)", attempts, err)
	}

	if blob.puts != 2 {
		t.Fatalf("blob saw %d puts, want 2", blob.puts)
	}

	// never fewer than one attempt
	blob = &flakyBlob{}
	attempts, err = putWithRetry(t.Context(), blob, []byte("x"), 0)
	if err != nil || attempts != 1 {
		t.Fatalf("putWithRetry with 0 attempts = (%d, %v), want (1, nil)", attempts, err)
	}
}

func TestPercentile(t *testing.T) {
	t.Parallel()

	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}

	durations := make([]time.Duration, 100)
	for i := range durations {
		durations[i] = time.Duration(i+1) * time.Millisecond
	}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0, 1 * time.Millisecond},
		{0.5, 50 * time.Millisecond},
		{0.95, 95 * time.Millisecond},
		{1, 100 * time.Millisecond},
	}

	for _, tc := range cases {
		if got := percentile(durations, tc.p); got != tc.want {
			t.Errorf("percentile(p=%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 << 30, "3.0 GiB"},
	}

	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSprayConcurrency(t *testing.T) {
	t.Parallel()

	if got := sprayConcurrency(0); got < 4 || got > 32 {
		t.Errorf("default = %d, want 4..32", got)
	}

	if got := sprayConcurrency(8); got != 8 {
		t.Errorf("explicit = %d, want 8", got)
	}

	if got := sprayConcurrency(1000); got != 256 {
		t.Errorf("cap = %d, want 256", got)
	}
}

func TestMigrateTargetVersion(t *testing.T) {
	t.Parallel()

	if got := migrateTargetVersion(header.MetadataVersionV4); got != header.MetadataVersionV4 {
		t.Errorf("v4 request = %d, want 4", got)
	}

	if got := migrateTargetVersion(header.MetadataVersionV5); got != header.MetadataVersionV5 {
		t.Errorf("v5 request = %d, want 5", got)
	}

	// Anything else (including a zero flag) means the current write version.
	for _, requested := range []uint64{0, 3, 99} {
		if got := migrateTargetVersion(requested); got != header.MetadataVersionV5 {
			t.Errorf("requested %d -> %d, want the current write version %d", requested, got, header.MetadataVersionV5)
		}
	}
}
