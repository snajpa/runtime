// Command s3-rehearsal drives one e2b version's storage layer against a shared
// object store, so a fleet of two versions can be rehearsed on live storage:
// write with one, read with the other, and prove nothing is stranded.
//
// It is built once per version of the runtime (see ../build.sh): the same
// source linked against an old checkout and a new checkout *is* the
// mixed-version fleet, with the storage and header code the only difference
// between the binaries.
//
// Every phase prints one JSON object on stdout so a caller can tell the
// outcomes apart:
//
//	ok        the phase did what the matrix expects
//	rejected  the version refused data it does not understand (loud, allowed)
//	misread   data came back wrong — the failure this rehearsal exists to catch
//	error     anything else (transport, credentials, disk)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// version is stamped at build time with
// -ldflags "-X main.version=<checkout>@<revision>".
var version = "dev"

const (
	blockSize = 4096
	codecZstd = "zstd"
	// production default frame size (2 MiB) and the S3 minimum part size, from
	// the storage tests; multipart uploads are part of what is rehearsed.
	frameSizeKB   = 2 * 1024
	minPartSizeMB = 5
)

type result struct {
	Phase    string  `json:"phase"`
	Version  string  `json:"version"`
	Outcome  string  `json:"outcome"`
	Detail   string  `json:"detail,omitempty"`
	Objects  int     `json:"objects,omitempty"`
	Bytes    int64   `json:"bytes,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
	Protocol string  `json:"protocol,omitempty"`
}

type entry struct {
	Build     string    `json:"build"`
	Payload   string    `json:"payload"`
	Header    string    `json:"header"`
	Size      int64     `json:"size"`
	Checksum  string    `json:"checksum"`
	Codec     string    `json:"codec"`
	WrittenBy string    `json:"written_by"`
	WrittenAt time.Time `json:"written_at"`
}

type manifest struct {
	Version    string    `json:"version"`
	StorageURL string    `json:"storage_url"`
	Prefix     string    `json:"prefix"`
	Entries    []entry   `json:"entries"`
	CreatedAt  time.Time `json:"created_at"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	args := os.Args[2:]

	switch os.Args[1] {
	case "ensure-bucket":
		ensureBucket(args)
	case "count":
		countObjects(args)
	case "spray":
		spray(args)
	case "tamper":
		tamper(args)
	case "purge":
		purge(args)
	case "migrate":
		migrate(args)
	case "probe":
		probe(args)
	case "write":
		write(args)
	case "read":
		read(args)
	case "exists":
		exists(args)
	case "prune":
		prune(args)
	case "versions":
		emit(result{
			Phase: "versions", Version: version, Outcome: "ok",
			Detail: fmt.Sprintf("go=%s cores=%d", runtime.Version(), runtime.NumCPU()),
		})
	default:
		usage()
	}
}

// readObject reads a whole logical payload the way a node does: chunk-aligned
// ranges through the cache layer's range reader, one reader per range so every
// frame's CRC is verified on Close. A reader may serve less than the requested
// range (a frame boundary), so the loop advances by what it actually read; the
// caller still checks the total length and the hash.
func readObject(ctx context.Context, provider storage.StorageProvider, path string, size int64, ft *storage.FrameTable) ([]byte, error) {
	seekable, err := provider.OpenSeekable(ctx, path)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, size)

	for off := int64(0); off < size; {
		length := min(int64(storage.MemoryChunkSize), size-off)

		reader, _, err := seekable.OpenRangeReader(ctx, off, length, ft)
		if err != nil {
			return nil, fmt.Errorf("range at %d: %w", off, err)
		}

		var chunk bytesBuffer
		_, copyErr := io.Copy(&chunk, reader)
		_, closeErr := reader.Close(ctx)

		switch {
		case copyErr != nil:
			return nil, fmt.Errorf("read at %d: %w", off, copyErr)
		case closeErr != nil:
			return nil, fmt.Errorf("frame verification at %d: %w", off, closeErr)
		case len(chunk.Bytes()) == 0:
			return nil, fmt.Errorf("read at %d made no progress", off)
		}

		out = append(out, chunk.Bytes()...)
		off += int64(len(chunk.Bytes()))
	}

	return out, nil
}

// newS3Client builds the SDK client used by the phases that need the S3 API
// itself (bucket creation, inventory): they work on the same spec the storage
// layer gets.
func newS3Client(spec storage.Spec) *s3.Client {
	region := spec.Region
	if region == "" {
		region = "us-east-1"
	}

	cfg := aws.Config{
		Credentials: credentials.NewStaticCredentialsProvider(
			os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
		Region: region,
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if spec.Endpoint != "" {
			o.BaseEndpoint = aws.String(spec.Endpoint)
		}

		o.UsePathStyle = spec.UsePathStyle
	})
}

// inventoryPrefix walks every object under a prefix through the S3 API and
// returns the count, the bytes and a per-suffix breakdown. It is what both the
// inventory leg and a destructive operation's dry run are built on.
func inventoryPrefix(ctx context.Context, client *s3.Client, bucket, prefix string) (int, int64, map[string]int, error) {
	var (
		objects  int
		bytes    int64
		bySuffix = map[string]int{}
		token    *string
	)

	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
			MaxKeys:           aws.Int32(1000),
		})
		if err != nil {
			return 0, 0, nil, err
		}

		for _, object := range out.Contents {
			objects++
			bytes += aws.ToInt64(object.Size)
			bySuffix[path.Ext(aws.ToString(object.Key))]++
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}

		token = out.NextContinuationToken
	}

	return objects, bytes, bySuffix, nil
}

// migrateTargetVersion accepts only header formats that exist (V4 and V5 both
// carry frame tables); anything else means the current write version.
func migrateTargetVersion(requested uint64) uint64 {
	switch requested {
	case header.MetadataVersionV4, header.MetadataVersionV5:
		return requested
	default:
		return header.MetadataVersionV5
	}
}

// countObjects is the inventory primitive the readiness note calls G1: how
// many artifacts exist under a prefix and how big they are, by suffix. A
// fleet-wide rollback or deprecation decision needs this before it can claim
// that nothing was stranded.
func countObjects(args []string) {
	fs := flag.NewFlagSet("count", flag.ExitOnError)
	storageURL := fs.String("storage-url", "", "storage URL (required)")
	prefix := fs.String("prefix", "", "prefix to inventory (required)")
	_ = fs.Parse(args)

	spec, err := storage.ParseStorageURL(*storageURL)
	if err != nil {
		emit(result{Phase: "count", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	if spec.Provider != storage.AWSStorageProvider {
		emit(result{
			Phase: "count", Version: version, Outcome: "error",
			Detail: fmt.Sprintf("inventory needs the S3 API; provider is %s", spec.Provider),
		})

		return
	}

	started := time.Now()
	client := newS3Client(spec)

	objects, bytes, bySuffix, err := inventoryPrefix(context.Background(), client, spec.Bucket, *prefix)
	if err != nil {
		emit(result{Phase: "count", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	suffixes := make([]string, 0, len(bySuffix))
	for suffix, n := range bySuffix {
		suffixes = append(suffixes, fmt.Sprintf("%s=%d", suffix, n))
	}

	slices.Sort(suffixes)

	emit(result{
		Phase: "count", Version: version, Outcome: "ok",
		Detail: fmt.Sprintf("%s: %d objects, %s, %s", *prefix, objects,
			humanBytes(bytes), strings.Join(suffixes, " ")),
		Objects: objects, Bytes: bytes, Seconds: time.Since(started).Seconds(),
	})
}

// sprayProfile sizes a spray to the machine: many small objects is the shape
// production actually holds (see the note's arithmetic on 10^10 chunks), and a
// dev box cannot hold all of them, so the counts scale with cores.
func sprayProfile(name string, count int, size int64) (int, int64) {
	var defaults int

	switch name {
	case "tiny":
		defaults = 500
	case "small":
		defaults = 2000
	case "big":
		defaults = 20000
	default: // auto
		defaults = min(max(runtime.NumCPU()*125, 500), 20000)
	}

	// explicit flags win over the profile
	if count <= 0 {
		count = defaults
	}

	if size <= 0 {
		size = 4 << 10
	}

	return count, size
}

// sprayConcurrency picks the worker count for a spray. Object stores are
// latency-bound, so a handful of workers lifts throughput a lot - which is what
// makes 100k-1M object runs possible - while a dev box should not open hundreds
// of sockets at once.
func sprayConcurrency(requested int) int {
	if requested > 0 {
		return min(requested, 256)
	}

	return min(max(runtime.NumCPU()/2, 4), 32)
}

// putWithRetry stores one object, retrying a failed write up to attempts times.
// The storage layer bounds every write with its own deadline (awsWriteTimeout,
// 30s); a store under sustained pressure can exceed it - the 1M-object ramp
// found exactly that at ~725k objects - and production retries such writes. The
// rehearsal retries too and reports how often it had to, so saturation stays
// visible instead of being hidden.
func putWithRetry(ctx context.Context, blob storage.Blob, data []byte, attempts int) (int, error) {
	if attempts < 1 {
		attempts = 1
	}

	var err error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err = blob.Put(ctx, data); err == nil {
			return attempt, nil
		}

		if attempt < attempts {
			select {
			case <-ctx.Done():
				return attempt, ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}

	return attempts, err
}

// retrySummary renders how often writes had to be retried, which is the
// saturation signal a large ramp must not hide.
func retrySummary(retries, objects int64) string {
	if retries == 0 {
		return "no retries"
	}

	pct := float64(retries) / float64(objects) * 100

	return fmt.Sprintf("%d retries (%.2f%% of writes)", retries, pct)
}

// retryBackoff is the pause before retry number attempt (1-based).
func retryBackoff(attempt int) time.Duration {
	return min(time.Duration(attempt)*250*time.Millisecond, 2*time.Second)
}

// percentile returns the p-quantile (0..1) of a sorted duration slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	idx := int(float64(len(sorted)-1) * p)
	idx = max(idx, 0)
	idx = min(idx, len(sorted)-1)

	return sorted[idx]
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// spray publishes many small objects and reports the tail latencies a fleet
// would feel per object; -cleanup measures the prefix delete that GC would do.
func spray(args []string) {
	fs := flag.NewFlagSet("spray", flag.ExitOnError)
	storageURL := fs.String("storage-url", "", "storage URL (required)")
	prefix := fs.String("prefix", "", "object prefix (required)")
	profileName := fs.String("profile", "auto", "tiny|small|big|auto")
	count := fs.Int("count", 0, "number of objects (overrides the profile)")
	size := fs.Int64("bytes", 0, "bytes per object (overrides the profile)")
	cleanup := fs.Bool("cleanup", false, "delete the sprayed objects afterwards")
	concurrency := fs.Int("concurrency", 0, "parallel writers (default: cores/2, 4..32)")
	retryAttempts := fs.Int("retry-attempts", 3, "attempts per object write before failing")
	_ = fs.Parse(args)

	started := time.Now()
	n, objectSize := sprayProfile(*profileName, *count, *size)

	provider, err := openStoreProvider(*storageURL)
	if err != nil {
		emit(result{Phase: "spray", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	ctx := context.Background()
	workers := sprayConcurrency(*concurrency)

	perWorker := make([][]time.Duration, workers)
	indexCh := make(chan int)
	errCh := make(chan error, 1)
	done := make(chan struct{})

	var (
		once    sync.Once
		wg      sync.WaitGroup
		retries atomic.Int64
	)

	report := func(err error) {
		once.Do(func() {
			errCh <- err
			close(done)
		})
	}

	for w := range workers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			local := make([]time.Duration, 0, n/workers+1)

			for i := range indexCh {
				blob, err := provider.OpenBlob(ctx, fmt.Sprintf("%s/objects/%06d.bin", *prefix, i))
				if err != nil {
					report(fmt.Errorf("object %d: %w", i, err))

					return
				}

				t0 := time.Now()

				attemptsUsed, err := putWithRetry(ctx, blob, payloadData(int64(i)+1, objectSize), *retryAttempts)
				if err != nil {
					report(fmt.Errorf("object %d after %d attempts: %w", i, attemptsUsed, err))

					return
				}

				retries.Add(int64(attemptsUsed - 1))
				local = append(local, time.Since(t0))
			}

			perWorker[w] = local
		}(w)
	}

produce:
	for i := range n {
		select {
		case indexCh <- i:
		case <-done:
			break produce
		}
	}

	close(indexCh)
	wg.Wait()

	select {
	case err := <-errCh:
		emit(result{Phase: "spray", Version: version, Outcome: "error", Detail: err.Error()})

		return
	default:
	}

	durations := make([]time.Duration, 0, n)
	for _, local := range perWorker {
		durations = append(durations, local...)
	}

	slices.Sort(durations)

	elapsed := time.Since(started)
	written := int64(n) * objectSize

	detail := fmt.Sprintf("wrote %d objects of %d B in %s with %d workers (%.0f objects/s, %s, p50 %s, p95 %s, p99 %s)",
		n, objectSize, elapsed.Round(time.Millisecond), workers, float64(n)/elapsed.Seconds(),
		retrySummary(retries.Load(), int64(n)),
		percentile(durations, 0.50).Round(time.Microsecond),
		percentile(durations, 0.95).Round(time.Microsecond),
		percentile(durations, 0.99).Round(time.Microsecond))

	if *cleanup {
		t0 := time.Now()
		if err := provider.DeleteObjectsWithPrefix(ctx, *prefix); err != nil {
			emit(result{
				Phase: "spray", Version: version, Outcome: "error",
				Detail: fmt.Sprintf("cleanup: %v", err),
			})

			return
		}

		detail += fmt.Sprintf("; deleted the prefix in %s", time.Since(t0).Round(time.Millisecond))
	}

	emit(result{
		Phase: "spray", Version: version, Outcome: "ok",
		Detail: detail, Objects: n, Bytes: written,
		Seconds: time.Since(started).Seconds(), Protocol: "small objects",
	})
}

// tamper overwrites an artifact's payload with different bytes of the same
// length: fault injection for the read path. Reading a tampered artifact must
// come back as a misread (whole-file checksum or a frame CRC on Close), never
// as valid data - silent corruption is the failure mode this checks for.
func tamper(args []string) {
	fs := flag.NewFlagSet("tamper", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to tamper with (required)")
	index := fs.Int("index", 0, "entry to tamper with")
	_ = fs.Parse(args)

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "tamper", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	if *index < 0 || *index >= len(m.Entries) {
		emit(result{
			Phase: "tamper", Version: version, Outcome: "error",
			Detail: fmt.Sprintf("index %d out of range (%d entries)", *index, len(m.Entries)),
		})

		return
	}

	entry := m.Entries[*index]

	provider, err := openProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "tamper", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	blob, err := provider.OpenBlob(context.Background(), entry.Payload)
	if err != nil {
		emit(result{Phase: "tamper", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	// Same length as the original, different content.
	if err := blob.Put(context.Background(), payloadData(9999, entry.Size)); err != nil {
		emit(result{
			Phase: "tamper", Version: version, Outcome: "error",
			Detail: fmt.Sprintf("overwrite: %v", err),
		})

		return
	}

	emit(result{
		Phase: "tamper", Version: version, Outcome: "ok",
		Detail:  fmt.Sprintf("overwrote %s (%d B) with different bytes", entry.Payload, entry.Size),
		Objects: 1, Bytes: entry.Size,
	})
}

// purge deletes every object under a prefix and reports the cost: what a
// lifecycle change, a rollback cleanup or a GC sweep would pay, measured on
// the same store the fleet uses.
func purge(args []string) {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	storageURL := fs.String("storage-url", "", "storage URL (required)")
	prefix := fs.String("prefix", "", "prefix to delete (required)")
	dryRun := fs.Bool("dry-run", false, "report what would be deleted without deleting")
	_ = fs.Parse(args)

	if *dryRun {
		spec, specErr := storage.ParseStorageURL(*storageURL)
		if specErr != nil || spec.Provider != storage.AWSStorageProvider {
			emit(result{
				Phase: "purge", Version: version, Outcome: "error",
				Detail: "dry run needs an s3:// storage URL",
			})

			return
		}

		started := time.Now()

		objects, bytes, _, listErr := inventoryPrefix(context.Background(), newS3Client(spec), spec.Bucket, *prefix)
		if listErr != nil {
			emit(result{Phase: "purge", Version: version, Outcome: "error", Detail: listErr.Error()})

			return
		}

		emit(result{
			Phase: "purge", Version: version, Outcome: "ok",
			Detail:   fmt.Sprintf("dry run: would delete %d objects, %s under %s", objects, humanBytes(bytes), *prefix),
			Objects:  objects,
			Bytes:    bytes,
			Seconds:  time.Since(started).Seconds(),
			Protocol: "dry run",
		})

		return
	}

	provider, err := openStoreProvider(*storageURL)
	if err != nil {
		emit(result{Phase: "purge", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	started := time.Now()

	if err := provider.DeleteObjectsWithPrefix(context.Background(), *prefix); err != nil {
		emit(result{Phase: "purge", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	emit(result{
		Phase: "purge", Version: version, Outcome: "ok",
		Detail:   fmt.Sprintf("deleted %s in %s", *prefix, time.Since(started).Round(time.Millisecond)),
		Seconds:  time.Since(started).Seconds(),
		Protocol: "prefix delete",
	})
}

// migrate rewrites the artifacts of a manifest in the target header format:
// the backfill primitive a lazy-rewrite or migration job needs to move existing
// objects onto the current write format without breaking older readers inside
// the compatibility window. Each artifact is read back and verified against the
// manifest checksum before it is rewritten.
func migrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to migrate (required)")
	requested := fs.Uint64("header-version", header.MetadataVersionV5, "target header format (4 or 5)")
	_ = fs.Parse(args)

	target := migrateTargetVersion(*requested)

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "migrate", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	provider, err := openStoreProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "migrate", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	started := time.Now()
	ctx := context.Background()

	workDir, err := os.MkdirTemp("", "s3-rehearsal-migrate")
	if err != nil {
		emit(result{Phase: "migrate", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}
	defer os.RemoveAll(workDir)

	var (
		migrated int
		bytes    int64
		refused  int
		misread  int
	)

	for i, e := range m.Entries {
		buildUUID, err := uuid.Parse(e.Build)
		if err != nil {
			refused++

			continue
		}

		loaded, _, err := header.LoadHeader(ctx, provider, e.Header)
		if err != nil {
			refused++

			continue
		}

		data, err := readObject(ctx, provider, e.Payload, e.Size, loaded.GetBuildFrameData(buildUUID))
		if err != nil {
			refused++

			continue
		}

		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != e.Checksum {
			misread++

			continue
		}

		localPath := filepath.Join(workDir, fmt.Sprintf("payload-%d.bin", i))
		if err := os.WriteFile(localPath, data, 0o600); err != nil {
			emit(result{Phase: "migrate", Version: version, Outcome: "error", Detail: err.Error()})

			return
		}

		seekable, err := provider.OpenSeekable(ctx, e.Payload)
		if err != nil {
			emit(result{Phase: "migrate", Version: version, Outcome: "error", Detail: err.Error()})

			return
		}

		fullFT, checksum, err := seekable.StoreFile(ctx, localPath, storage.WithCompressConfig(compressConfig()))
		if err != nil {
			emit(result{
				Phase: "migrate", Version: version, Outcome: "error",
				Detail: fmt.Sprintf("rewrite %s: %v", e.Build, err),
			})

			return
		}

		metadata := header.NewTemplateMetadata(buildUUID, blockSize, uint64(len(data)))

		spec, err := header.NewHeader(metadata, []header.BuildMap{{
			Offset:             0,
			Length:             uint64(len(data)),
			BuildId:            buildUUID,
			BuildStorageOffset: 0,
		}})
		if err != nil {
			emit(result{Phase: "migrate", Version: version, Outcome: "error", Detail: "header: " + err.Error()})

			return
		}

		spec.SetBuild(buildUUID, header.BuildData{
			Size:      int64(len(data)),
			Checksum:  checksum,
			FrameData: fullFT.Table(),
		})

		if _, _, _, err := header.StoreHeader(ctx, provider, e.Header, spec.CloneForUpload(target)); err != nil {
			emit(result{
				Phase: "migrate", Version: version, Outcome: "error",
				Detail: fmt.Sprintf("store header %s: %v", e.Build, err),
			})

			return
		}

		migrated++
		bytes += int64(len(data))
	}

	outcome := "ok"
	detail := fmt.Sprintf("rewrote %d artifacts in header v%d (%s) in %s", migrated, target,
		humanBytes(bytes), time.Since(started).Round(time.Millisecond))

	switch {
	case misread > 0:
		outcome = "misread"
		detail = fmt.Sprintf("%d artifacts failed verification before rewrite; %d rewritten", misread, migrated)
	case refused > 0:
		detail += fmt.Sprintf("; %d refused", refused)
	}

	emit(result{
		Phase: "migrate", Version: version, Outcome: outcome, Detail: detail,
		Objects: migrated, Bytes: bytes, Seconds: time.Since(started).Seconds(),
		Protocol: fmt.Sprintf("header v%d", target),
	})
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: s3-rehearsal <phase> [flags]

phases:
  probe     write and read back one small object
  write     publish a set of artifacts and write a manifest
  read      read and verify every artifact in a manifest
  exists    check every artifact in a manifest still exists
  prune     delete the artifacts of a manifest (rollback/GC leg)
  versions  print the stamped version
  count     inventory an object prefix (count and bytes, S3 ListObjectsV2)
  spray     publish many small objects with tail latencies (-cleanup deletes)
  tamper    overwrite one artifact with different bytes (fault injection)
  purge     delete every object under a prefix, timing the GC/delete path
            (-dry-run reports what would go, without deleting)
  migrate   rewrite a manifest's artifacts in a target header format (backfill)

flags: --storage-url, --prefix, --manifest, --profile, --builds, --bytes, --codec
`)
	os.Exit(2)
}

type profile struct {
	builds  int
	payload int64
}

// pickProfile sizes a run for the box it is on: the same harness has to be
// meaningful on a small dev machine and on a big node, so "auto" scales with
// cores and memory and the explicit profiles pin a shape for comparisons.
func pickProfile(name string, builds int, payload int64) profile {
	var p profile

	switch name {
	case "tiny":
		p = profile{builds: 2, payload: 4 << 20}
	case "small":
		p = profile{builds: 4, payload: 16 << 20}
	case "big":
		p = profile{builds: 16, payload: 256 << 20}
	default: // auto
		p = profile{
			builds:  min(max(runtime.NumCPU()/2, 2), 32),
			payload: min(max((totalMemGB()/4)<<20, 4<<20), 256<<20),
		}
	}

	if builds > 0 {
		p.builds = builds
	}

	if payload > 0 {
		p.payload = payload
	}

	return p
}

func totalMemGB() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 8
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}

		if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			return kb / 1024 / 1024
		}

		break
	}

	return 8
}

// openStoreProvider is the provider without the chunk cache. The object-count
// legs measure the store itself; caching every small object locally would write
// a chunk file per object and confound both the measurement and the cache.
func openStoreProvider(rawURL string) (storage.StorageProvider, error) {
	spec, err := storage.ParseStorageURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse storage url: %w", err)
	}

	return storage.NewProvider(context.Background(), spec)
}

// openProvider builds the provider a node runs with: the object store wrapped
// in the NFS chunk cache. The wrapping matters for correctness of the
// rehearsal, not just for fidelity — the cache layer's range reader walks
// frame tables, the raw provider's reader does not. The cache directory is per
// invocation unless S3_REHEARSAL_CACHE_DIR says otherwise, so a warm local
// cache cannot mask a store problem.
func openProvider(rawURL string) (storage.StorageProvider, error) {
	spec, err := storage.ParseStorageURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse storage url: %w", err)
	}

	ctx := context.Background()

	inner, err := storage.NewProvider(ctx, spec)
	if err != nil {
		return nil, err
	}

	cacheDir := os.Getenv("S3_REHEARSAL_CACHE_DIR")
	if cacheDir == "" {
		cacheDir, err = os.MkdirTemp("", "s3-rehearsal-cache")
		if err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}

	flags, err := featureflags.NewClient()
	if err != nil {
		return nil, fmt.Errorf("feature flags: %w", err)
	}

	return storage.WrapInNFSCache(ctx, cacheDir, inner, flags), nil
}

// ensureBucket creates the bucket if it is missing. The storage layer assumes
// the bucket exists (the repo's own tests create it explicitly), and a
// rehearsal should not need hand-run setup, so this uses the SDK directly
// against the same spec the provider gets.
func ensureBucket(args []string) {
	fs := flag.NewFlagSet("ensure-bucket", flag.ExitOnError)
	storageURL := fs.String("storage-url", "", "storage URL (required)")
	_ = fs.Parse(args)

	spec, err := storage.ParseStorageURL(*storageURL)
	if err != nil {
		emit(result{Phase: "ensure-bucket", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	if spec.Provider != storage.AWSStorageProvider {
		emit(result{
			Phase: "ensure-bucket", Version: version, Outcome: "ok",
			Detail: fmt.Sprintf("provider %s manages its own buckets", spec.Provider),
		})

		return
	}

	client := newS3Client(spec)

	_, err = client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(spec.Bucket)})
	if err != nil {
		exists, headErr := client.HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String(spec.Bucket)})

		if headErr != nil || exists == nil {
			emit(result{Phase: "ensure-bucket", Version: version, Outcome: "error", Detail: err.Error()})

			return
		}
	}

	emit(result{
		Phase: "ensure-bucket", Version: version, Outcome: "ok",
		Detail: fmt.Sprintf("bucket %s ready at %s", spec.Bucket, spec.Endpoint),
	})
}

func compressConfig() storage.CompressConfig {
	return storage.CompressConfig{
		Enabled:            true,
		Type:               codecZstd,
		Level:              2,
		FrameSizeKB:        frameSizeKB,
		MinPartSizeMB:      minPartSizeMB,
		FrameEncodeWorkers: max(runtime.NumCPU()/4, 1),
		EncoderConcurrency: 1,
	}
}

// payloadData generates deterministic, semi-compressible bytes so a mismatch is
// detectable and the frame tables look like real data.
func payloadData(seed int64, size int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	buf := make([]byte, size)

	for i := range buf {
		if i%64 < 48 {
			buf[i] = byte(rng.Intn(16))
		} else {
			buf[i] = byte(rng.Intn(256))
		}
	}

	return buf
}

func buildID(prefix string, index int) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(prefix+"/"+strconv.Itoa(index)))
}

func probe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	storageURL := fs.String("storage-url", "", "storage URL (required)")
	prefix := fs.String("prefix", "probe", "object prefix")
	_ = fs.Parse(args)

	started := time.Now()

	provider, err := openProvider(*storageURL)
	if err != nil {
		emit(result{Phase: "probe", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	ctx := context.Background()
	objectPath := *prefix + "/probe.bin"
	payload := payloadData(1, 1<<20)
	want := sha256.Sum256(payload)

	local := filepath.Join(os.TempDir(), "s3-rehearsal-probe.bin")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		emit(result{Phase: "probe", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}
	defer os.Remove(local)

	seekable, err := provider.OpenSeekable(ctx, objectPath)
	if err != nil {
		emit(result{Phase: "probe", Version: version, Outcome: "error", Detail: "open: " + err.Error()})

		return
	}

	fullFT, _, err := seekable.StoreFile(ctx, local, storage.WithCompressConfig(compressConfig()))
	if err != nil {
		emit(result{Phase: "probe", Version: version, Outcome: "error", Detail: "store: " + err.Error()})

		return
	}

	data, err := readObject(ctx, provider, objectPath, int64(len(payload)), fullFT.Table())
	if err != nil {
		emit(result{Phase: "probe", Version: version, Outcome: "misread", Detail: "read back: " + err.Error()})

		return
	}

	var got bytesBuffer
	_, _ = got.Write(data)

	gotSum := sha256.Sum256(got.Bytes())
	outcome, detail := "ok", "round trip verified"

	if gotSum != want {
		outcome = "misread"
		detail = fmt.Sprintf("payload mismatch: wrote %s, read %s",
			hex.EncodeToString(want[:8]), hex.EncodeToString(gotSum[:8]))
	}

	if measure, err := provider.OpenSeekable(ctx, objectPath); err == nil {
		if size, sizeErr := measure.Size(ctx); sizeErr == nil && size != int64(len(payload)) {
			outcome = "misread"
			detail = fmt.Sprintf("size mismatch: wrote %d, read %d", len(payload), size)
		}
	}

	emit(result{
		Phase: "probe", Version: version, Outcome: outcome, Detail: detail,
		Objects: 1, Bytes: int64(len(payload)), Seconds: time.Since(started).Seconds(),
		Protocol: "zstd",
	})
}

func write(args []string) {
	fs := flag.NewFlagSet("write", flag.ExitOnError)
	storageURL := fs.String("storage-url", "", "storage URL (required)")
	prefix := fs.String("prefix", "", "object prefix (required)")
	manifestPath := fs.String("manifest", "", "where to write the manifest (required)")
	profileName := fs.String("profile", "auto", "tiny|small|big|auto")
	builds := fs.Int("builds", 0, "number of builds (overrides the profile)")
	payload := fs.Int64("bytes", 0, "payload bytes per build (overrides the profile)")
	headerVersion := fs.Uint64("header-version", header.MetadataVersionV5, "header format version to write (4 or 5)")
	layout := fs.String("layout", layoutFlat, "flat (harness paths) | product (storage.Paths layout)")
	_ = fs.Parse(args)

	started := time.Now()
	prof := pickProfile(*profileName, *builds, *payload)

	provider, err := openProvider(*storageURL)
	if err != nil {
		emit(result{Phase: "write", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	ctx := context.Background()
	m := manifest{Version: version, StorageURL: *storageURL, Prefix: *prefix, CreatedAt: time.Now()}

	workDir, err := os.MkdirTemp("", "s3-rehearsal")
	if err != nil {
		emit(result{Phase: "write", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}
	defer os.RemoveAll(workDir)

	for i := range prof.builds {
		id := buildID(*prefix, i)
		localPayload := filepath.Join(workDir, fmt.Sprintf("payload-%d.bin", i))
		data := payloadData(int64(i)+1, prof.payload)

		if err := os.WriteFile(localPayload, data, 0o600); err != nil {
			emit(result{Phase: "write", Version: version, Outcome: "error", Detail: err.Error()})

			return
		}

		plans, err := artifactPlans(*layout, *prefix, id)
		if err != nil {
			emit(result{Phase: "write", Version: version, Outcome: "error", Detail: err.Error()})

			return
		}

		for _, plan := range plans {
			written, err := writeArtifactPlan(ctx, provider, id, plan, localPayload, data, prof.payload, *headerVersion)
			if err != nil {
				emit(result{
					Phase: "write", Version: version, Outcome: "error",
					Detail: fmt.Sprintf("store build %d %s: %v", i, plan.name, err),
				})

				return
			}

			m.Entries = append(m.Entries, written)
		}
	}

	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		emit(result{Phase: "write", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	if err := os.WriteFile(*manifestPath, append(encoded, '\n'), 0o600); err != nil {
		emit(result{Phase: "write", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	emit(result{
		Phase: "write", Version: version, Outcome: "ok",
		Detail: fmt.Sprintf("wrote %d artifacts (%d builds, %.1f MiB each, %s) to %s",
			len(m.Entries), prof.builds, float64(prof.payload)/float64(1<<20), *layout, *prefix),
		Objects:  2 * len(m.Entries),
		Bytes:    int64(len(m.Entries)) * prof.payload,
		Seconds:  time.Since(started).Seconds(),
		Protocol: fmt.Sprintf("zstd/%dKB frames", frameSizeKB),
	})
}

// Layouts the rehearsal can write artifacts in.
const (
	// layoutFlat is the harness's own shape: one artifact per build under the
	// run prefix.
	layoutFlat = "flat"
	// layoutProduct is what the runtime's storage.Paths describe, so the
	// runtime's own tooling (migrate-builds) can operate on the artifacts the
	// rehearsal produced.
	layoutProduct = "product"
)

// artifactPlan is where one artifact's payload and header live.
type artifactPlan struct {
	name    string
	payload string
	header  string
}

// artifactPlans returns the artifacts to write for one build.
func artifactPlans(layout, prefix string, id uuid.UUID) ([]artifactPlan, error) {
	switch layout {
	case layoutFlat:
		payload := fmt.Sprintf("%s/builds/%s/%s", prefix, id, storage.RootfsName)

		return []artifactPlan{{name: storage.RootfsName, payload: payload, header: payload + storage.HeaderSuffix}}, nil
	case layoutProduct:
		paths := storage.Paths{BuildID: id.String()}

		names := []string{storage.RootfsName, storage.MemfileName}
		plans := make([]artifactPlan, 0, len(names))

		for _, name := range names {
			plans = append(plans, artifactPlan{
				name:    name,
				payload: paths.DataFile(name, storage.CompressionZstd),
				header:  paths.HeaderFile(name),
			})
		}

		return plans, nil
	default:
		return nil, fmt.Errorf("unknown layout %q (want %s or %s)", layout, layoutFlat, layoutProduct)
	}
}

// writeArtifactPlan stores one artifact's payload and header and returns the
// manifest entry describing it.
func writeArtifactPlan(ctx context.Context, provider storage.StorageProvider, id uuid.UUID, plan artifactPlan, localPayload string, data []byte, size int64, headerVersion uint64) (entry, error) {
	seekable, err := provider.OpenSeekable(ctx, plan.payload)
	if err != nil {
		return entry{}, fmt.Errorf("open: %w", err)
	}

	fullFT, checksum, err := seekable.StoreFile(ctx, localPayload, storage.WithCompressConfig(compressConfig()))
	if err != nil {
		return entry{}, err
	}

	if checksum != sha256.Sum256(data) {
		return entry{}, errors.New("storefile checksum mismatch")
	}

	metadata := header.NewTemplateMetadata(id, blockSize, uint64(size))

	spec, err := header.NewHeader(metadata, []header.BuildMap{{
		Offset:             0,
		Length:             uint64(size),
		BuildId:            id,
		BuildStorageOffset: 0,
	}})
	if err != nil {
		return entry{}, fmt.Errorf("header: %w", err)
	}

	spec.SetBuild(id, header.BuildData{
		Size:      size,
		Checksum:  checksum,
		FrameData: fullFT.Table(),
	})

	// A compressed artifact carries its frame tables in the header, and only the
	// V4/V5 formats serialize them: production promotes the header to the write
	// version before storing it.
	upload := spec.CloneForUpload(headerVersion)

	if _, _, _, err := header.StoreHeader(ctx, provider, plan.header, upload); err != nil {
		return entry{}, fmt.Errorf("store header: %w", err)
	}

	return entry{
		Build:     id.String(),
		Payload:   plan.payload,
		Header:    plan.header,
		Size:      size,
		Checksum:  hex.EncodeToString(checksum[:]),
		Codec:     codecZstd,
		WrittenBy: version,
		WrittenAt: time.Now(),
	}, nil
}

func read(args []string) {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to read (required)")
	_ = fs.Parse(args)

	started := time.Now()

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "read", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	provider, err := openProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "read", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	ctx := context.Background()

	var (
		ok, rejected, misread int
		rejectDetail          string
		misreadDetail         string
		bytes                 int64
		headerVersions        = map[uint64]int{}
		writtenBy             = map[string]struct{}{}
	)

	for _, e := range m.Entries {
		writtenBy[e.WrittenBy] = struct{}{}

		loaded, _, err := header.LoadHeader(ctx, provider, e.Header)
		if err != nil {
			rejected++
			if rejectDetail == "" {
				rejectDetail = fmt.Sprintf("%s: %v", e.Build, err)
			}

			continue
		}

		buildUUID, err := uuid.Parse(e.Build)
		if err != nil {
			misread++
			misreadDetail = "bad build id in manifest: " + err.Error()

			continue
		}

		seekable, err := provider.OpenSeekable(ctx, e.Payload)
		if err != nil {
			rejected++
			if rejectDetail == "" {
				rejectDetail = fmt.Sprintf("%s: %v", e.Build, err)
			}

			continue
		}

		// Reads go through the frame table the header carries, which is what
		// production does for compressed artifacts.
		ft := loaded.GetBuildFrameData(buildUUID)
		frameSummary := "no frame table"
		if ft != nil {
			frameSummary = fmt.Sprintf("%d frames, uncompressed=%d, compressed=%d",
				ft.NumFrames(), ft.UncompressedSize(), ft.CompressedSize())
		}

		objectSize, sizeErr := seekable.Size(ctx)
		if sizeErr != nil {
			objectSize = -1
		}

		data, err := readObject(ctx, provider, e.Payload, e.Size, ft)
		if err != nil {
			rejected++
			if rejectDetail == "" {
				rejectDetail = fmt.Sprintf("%s: %v", e.Build, err)
			}

			continue
		}

		var buf bytesBuffer
		_, _ = buf.Write(data)

		want, err := hex.DecodeString(e.Checksum)
		if err != nil {
			misread++
			misreadDetail = "bad manifest checksum: " + err.Error()

			continue
		}

		got := sha256.Sum256(buf.Bytes())
		if string(got[:]) != string(want) || int64(len(buf.Bytes())) != e.Size {
			misread++
			if misreadDetail == "" {
				misreadDetail = fmt.Sprintf("%s: wrote %d bytes %s, read %d bytes %s "+
					"(header v%d, %s, object size=%d)",
					e.Build, e.Size, e.Checksum[:16], len(buf.Bytes()), hex.EncodeToString(got[:8]),
					loaded.Metadata.Version, frameSummary, objectSize)
			}

			continue
		}

		headerVersions[loaded.Metadata.Version]++
		bytes += int64(len(buf.Bytes()))
		ok++
	}

	writers := make([]string, 0, len(writtenBy))
	for w := range writtenBy {
		writers = append(writers, w)
	}
	slices.Sort(writers)

	versionList := make([]string, 0, len(headerVersions))
	for v, n := range headerVersions {
		versionList = append(versionList, fmt.Sprintf("v%d×%d", v, n))
	}
	slices.Sort(versionList)

	outcome := "ok"
	detail := fmt.Sprintf("read %d/%d artifacts written by %s (header versions %s)",
		ok, len(m.Entries), strings.Join(writers, ","), strings.Join(versionList, " "))

	switch {
	case misread > 0:
		outcome = "misread"
		detail = fmt.Sprintf("%d of %d MISREAD: %s", misread, len(m.Entries), misreadDetail)
	case rejected == len(m.Entries) && rejected > 0:
		outcome = "rejected"
		detail = fmt.Sprintf("all %d refused: %s", rejected, rejectDetail)
	case rejected > 0:
		detail = fmt.Sprintf("%d read, %d refused loudly: %s", ok, rejected, rejectDetail)
	}

	emit(result{
		Phase: "read", Version: version, Outcome: outcome, Detail: detail,
		Objects: ok, Bytes: bytes, Seconds: time.Since(started).Seconds(),
	})
}

func exists(args []string) {
	fs := flag.NewFlagSet("exists", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to check (required)")
	_ = fs.Parse(args)

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "exists", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	provider, err := openProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "exists", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	ctx := context.Background()

	var (
		count   int
		missing []string
	)

	for _, e := range m.Entries {
		for _, objectPath := range []string{e.Payload, e.Header} {
			blob, err := provider.OpenBlob(ctx, objectPath)
			if err != nil {
				missing = append(missing, objectPath)

				continue
			}

			found, err := blob.Exists(ctx)
			if err != nil || !found {
				missing = append(missing, objectPath)

				continue
			}

			count++
		}
	}

	outcome := "ok"
	detail := fmt.Sprintf("%d of %d objects present", count, 2*len(m.Entries))

	if len(missing) > 0 {
		outcome = "error"
		detail = fmt.Sprintf("%d objects missing, first: %s", len(missing), missing[0])
	}

	emit(result{Phase: "exists", Version: version, Outcome: outcome, Detail: detail, Objects: count})
}

func prune(args []string) {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to delete (required)")
	_ = fs.Parse(args)

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "prune", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	provider, err := openProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "prune", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	ctx := context.Background()

	for _, e := range m.Entries {
		prefix := strings.TrimSuffix(e.Payload, "/rootfs.ext4")
		if err := provider.DeleteObjectsWithPrefix(ctx, prefix); err != nil {
			emit(result{
				Phase: "prune", Version: version, Outcome: "error",
				Detail: fmt.Sprintf("delete %s: %v", prefix, err),
			})

			return
		}
	}

	emit(result{
		Phase: "prune", Version: version, Outcome: "ok",
		Detail: fmt.Sprintf("deleted %d builds", len(m.Entries)), Objects: 2 * len(m.Entries),
	})
}

func loadManifest(path string) (manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest: %w", err)
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("parse manifest: %w", err)
	}

	return m, nil
}

func emit(r result) {
	encoded, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal result: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(encoded))

	if r.Outcome == "misread" {
		os.Exit(3)
	}

	if r.Outcome == "error" {
		os.Exit(1)
	}
}

// bytesBuffer accumulates what WriteTo hands over so the rehearsal can hash it.
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)

	return len(p), nil
}

func (b *bytesBuffer) Bytes() []byte { return b.data }

var _ io.Writer = (*bytesBuffer)(nil)
