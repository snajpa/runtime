// Command s3-rehearsal-peer rehearses cross-node prefetch with real node
// processes: one process serves a build's artifacts over the repository's
// chunk service protocol, another fetches ranges from it and verifies them,
// and the same ranges are read locally from the object store for a baseline.
//
// What is the repository's real code here: the generated chunk service and its
// client stubs (packages/shared/pkg/grpc/orchestrator), the four RPCs the
// orchestrator's peer server answers (packages/orchestrator/pkg/server/chunks.go)
// and the storage read path (cache-wrapped provider, chunk-aligned range
// readers with frame tables and CRC-on-Close verification).
//
// What the harness supplies instead of production: the source resolution. A
// production peer answers from its template cache (peerserver.ResolveSeekable
// over the orchestrator's Cache); this harness resolves the same artifacts
// directly from the object store through a manifest, because the rehearsal
// writes them there itself. The RPC surface, the framing and the verification
// are therefore real, while the cache lookup is not - recording that difference
// is part of the point.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// version is stamped at build time with
// -ldflags "-X main.version=<checkout>@<revision>".
var version = "dev"

// streamMessageSize is what the server puts in one stream message.
const streamMessageSize = 1 << 20

type entry struct {
	Build    string `json:"build"`
	Payload  string `json:"payload"`
	Header   string `json:"header"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
}

type manifest struct {
	Version    string  `json:"version"`
	StorageURL string  `json:"storage_url"`
	Prefix     string  `json:"prefix"`
	Entries    []entry `json:"entries"`
}

type result struct {
	Phase   string  `json:"phase"`
	Version string  `json:"version"`
	Outcome string  `json:"outcome"`
	Detail  string  `json:"detail,omitempty"`
	Objects int     `json:"objects,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	args := os.Args[2:]

	switch os.Args[1] {
	case "serve":
		serve(args)
	case "fetch":
		fetch(args)
	case "versions":
		emit(result{
			Phase: "peer-versions", Version: version, Outcome: "ok",
			Detail: fmt.Sprintf("go=%s cores=%d", runtime.Version(), runtime.NumCPU()),
		})
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: s3-rehearsal-peer <phase> [flags]

phases:
  serve  answer chunk-service reads for a manifest's artifacts
  fetch  read a peer's artifacts over the chunk service, verify them, and
         compare against reading the same ranges from the object store

flags: --manifest, --addr, --peer, --bytes
`)
	os.Exit(2)
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

func (m manifest) entry(buildID string) (entry, bool) {
	for _, e := range m.Entries {
		if e.Build == buildID {
			return e, true
		}
	}

	return entry{}, false
}

// planRanges returns the offsets a fetch walks for a file of size bytes, in
// steps of chunk (the last range may be shorter).
func planRanges(size, chunk int64) []int64 {
	if size <= 0 || chunk <= 0 {
		return nil
	}

	offsets := make([]int64, 0, size/chunk+1)
	for off := int64(0); off < size; off += chunk {
		offsets = append(offsets, off)
	}

	return offsets
}

// openProvider mirrors the node read path: the store wrapped in the chunk
// cache. The cache directory is per process, so one node's warm cache can never
// mask what another node's reads look like.
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

	cacheDir, err := os.MkdirTemp("", "s3-rehearsal-peer-cache")
	if err != nil {
		return nil, err
	}

	flags, err := featureflags.NewClient()
	if err != nil {
		return nil, fmt.Errorf("feature flags: %w", err)
	}

	return storage.WrapInNFSCache(ctx, cacheDir, inner, flags), nil
}

func frameTable(ctx context.Context, provider storage.StorageProvider, headerPath string, buildID uuid.UUID) (*storage.FrameTable, error) {
	loaded, _, err := header.LoadHeader(ctx, provider, headerPath)
	if err != nil {
		return nil, err
	}

	return loaded.GetBuildFrameData(buildID), nil
}

// readRange reads [off, off+length) the way a node does: chunk-aligned range
// readers whose Close verifies every frame's CRC. The requested range itself
// may be unaligned (callers read partial chunks), so the enclosing aligned
// window is read and the requested slice returned.
func readRange(ctx context.Context, provider storage.StorageProvider, path string, off, length int64, ft *storage.FrameTable) ([]byte, error) {
	start, end := alignedWindow(off, length)

	data, err := readAligned(ctx, provider, path, start, end-start, ft)
	if err != nil {
		return nil, err
	}

	if int64(len(data)) < off-start+length {
		return nil, fmt.Errorf("read %d..%d returned only %d bytes", off, off+length, len(data))
	}

	return data[off-start : off-start+length], nil
}

// alignedWindow returns the chunk-aligned window that encloses [off, off+length),
// which is what the cache layer's range reader needs; the caller then slices the
// requested part out of it.
func alignedWindow(off, length int64) (int64, int64) {
	chunk := int64(storage.MemoryChunkSize)
	start := off - off%chunk
	end := (off + length + chunk - 1) / chunk * chunk

	return start, end
}

// readAligned reads a chunk-aligned window.
func readAligned(ctx context.Context, provider storage.StorageProvider, path string, off, length int64, ft *storage.FrameTable) ([]byte, error) {
	seekable, err := provider.OpenSeekable(ctx, path)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, length)

	for read := int64(0); read < length; {
		step := min(int64(storage.MemoryChunkSize), length-read)

		reader, _, err := seekable.OpenRangeReader(ctx, off+read, step, ft)
		if err != nil {
			return nil, fmt.Errorf("range at %d: %w", off+read, err)
		}

		var chunk bytesBuffer
		_, copyErr := io.Copy(&chunk, reader)
		_, closeErr := reader.Close(ctx)

		switch {
		case copyErr != nil:
			return nil, fmt.Errorf("read at %d: %w", off+read, copyErr)
		case closeErr != nil:
			return nil, fmt.Errorf("frame verification at %d: %w", off+read, closeErr)
		case len(chunk.Bytes()) == 0:
			return nil, fmt.Errorf("read at %d made no progress", off+read)
		}

		out = append(out, chunk.Bytes()...)
		read += int64(len(chunk.Bytes()))
	}

	return out, nil
}

// chunkServer implements the repository's chunk service the way the
// orchestrator's peer server does, resolving its sources from the object store
// through the manifest instead of the template cache.
type chunkServer struct {
	orchestrator.UnimplementedChunkServiceServer

	manifest manifest
	provider storage.StorageProvider
}

func (s *chunkServer) GetBuildFileSize(_ context.Context, req *orchestrator.GetBuildFileSizeRequest) (*orchestrator.GetBuildFileSizeResponse, error) {
	e, ok := s.manifest.entry(req.GetBuildId())
	if !ok {
		return &orchestrator.GetBuildFileSizeResponse{
			Availability: &orchestrator.PeerAvailability{NotAvailable: true},
		}, nil
	}

	return &orchestrator.GetBuildFileSizeResponse{TotalSize: e.Size}, nil
}

func (s *chunkServer) GetBuildFileExists(_ context.Context, req *orchestrator.GetBuildFileExistsRequest) (*orchestrator.GetBuildFileExistsResponse, error) {
	if _, ok := s.manifest.entry(req.GetBuildId()); !ok {
		return &orchestrator.GetBuildFileExistsResponse{
			Availability: &orchestrator.PeerAvailability{NotAvailable: true},
		}, nil
	}

	return &orchestrator.GetBuildFileExistsResponse{}, nil
}

func (s *chunkServer) ReadAtBuildSeekable(req *orchestrator.ReadAtBuildSeekableRequest, stream grpc.ServerStreamingServer[orchestrator.ReadAtBuildSeekableResponse]) error {
	ctx := stream.Context()

	e, ok := s.manifest.entry(req.GetBuildId())
	if !ok {
		return fmt.Errorf("build %s is not served here", req.GetBuildId())
	}

	if req.GetOffset() < 0 || req.GetLength() < 0 {
		return errors.New("offset and length must be non-negative")
	}

	buildUUID, err := uuid.Parse(e.Build)
	if err != nil {
		return fmt.Errorf("build id: %w", err)
	}

	ft, err := frameTable(ctx, s.provider, e.Header, buildUUID)
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}

	data, err := readRange(ctx, s.provider, e.Payload, req.GetOffset(), req.GetLength(), ft)
	if err != nil {
		return err
	}

	for start := 0; start < len(data); start += streamMessageSize {
		if err := stream.Send(&orchestrator.ReadAtBuildSeekableResponse{
			Data: data[start:min(start+streamMessageSize, len(data))],
		}); err != nil {
			return err
		}
	}

	return nil
}

func (s *chunkServer) GetBuildBlob(req *orchestrator.GetBuildBlobRequest, stream grpc.ServerStreamingServer[orchestrator.GetBuildBlobResponse]) error {
	e, ok := s.manifest.entry(req.GetBuildId())
	if !ok {
		return fmt.Errorf("build %s is not served here", req.GetBuildId())
	}

	blob, err := s.provider.OpenBlob(stream.Context(), e.Payload)
	if err != nil {
		return err
	}

	var buf bytesBuffer
	if _, err := blob.WriteTo(stream.Context(), &buf); err != nil {
		return err
	}

	data := buf.Bytes()
	for start := 0; start < len(data); start += streamMessageSize {
		if err := stream.Send(&orchestrator.GetBuildBlobResponse{
			Data: data[start:min(start+streamMessageSize, len(data))],
		}); err != nil {
			return err
		}
	}

	return nil
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to serve (required)")
	addr := fs.String("addr", "127.0.0.1:9101", "listen address")
	_ = fs.Parse(args)

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "peer-serve", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	provider, err := openProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "peer-serve", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *addr)
	if err != nil {
		emit(result{Phase: "peer-serve", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	server := grpc.NewServer()
	orchestrator.RegisterChunkServiceServer(server, &chunkServer{manifest: m, provider: provider})

	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-stopping
		server.GracefulStop()
	}()

	emit(result{
		Phase: "peer-serve", Version: version, Outcome: "ok",
		Detail: fmt.Sprintf("serving %d builds from %s on %s", len(m.Entries), m.StorageURL, *addr),
	})

	if err := server.Serve(listener); err != nil {
		emit(result{Phase: "peer-serve", Version: version, Outcome: "error", Detail: err.Error()})
	}
}

func fetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "manifest to fetch (required)")
	peer := fs.String("peer", "127.0.0.1:9101", "peer address")
	chunk := fs.Int64("bytes", int64(storage.MemoryChunkSize), "range size per request")
	_ = fs.Parse(args)

	m, err := loadManifest(*manifestPath)
	if err != nil {
		emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	local, err := openProvider(m.StorageURL)
	if err != nil {
		emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}

	conn, err := grpc.NewClient(*peer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: err.Error()})

		return
	}
	defer conn.Close()

	client := orchestrator.NewChunkServiceClient(conn)
	ctx := context.Background()

	var (
		peerTimes  []time.Duration
		localTimes []time.Duration
		bytesRead  int64
	)

	for _, e := range m.Entries {
		buildUUID, err := uuid.Parse(e.Build)
		if err != nil {
			emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: err.Error()})

			return
		}

		sizeResp, err := client.GetBuildFileSize(ctx, &orchestrator.GetBuildFileSizeRequest{
			BuildId: e.Build,
			Name:    "rootfs.ext4",
		})
		if err != nil {
			emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: "size: " + err.Error()})

			return
		}

		if sizeResp.GetAvailability().GetNotAvailable() {
			emit(result{
				Phase: "peer-fetch", Version: version, Outcome: "rejected",
				Detail: fmt.Sprintf("%s is not available on the peer", e.Build),
			})

			return
		}

		if sizeResp.GetTotalSize() != e.Size {
			emit(result{
				Phase: "peer-fetch", Version: version, Outcome: "misread",
				Detail: fmt.Sprintf("%s: peer reports %d bytes, manifest says %d",
					e.Build, sizeResp.GetTotalSize(), e.Size),
			})

			return
		}

		ft, err := frameTable(ctx, local, e.Header, buildUUID)
		if err != nil {
			emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: "header: " + err.Error()})

			return
		}

		sum := sha256.New()

		for _, off := range planRanges(e.Size, *chunk) {
			length := min(*chunk, e.Size-off)

			t0 := time.Now()

			stream, err := client.ReadAtBuildSeekable(ctx, &orchestrator.ReadAtBuildSeekableRequest{
				BuildId: e.Build,
				Name:    "rootfs.ext4",
				Offset:  off,
				Length:  length,
			})
			if err != nil {
				emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: "stream: " + err.Error()})

				return
			}

			var fromPeer bytesBuffer

			for {
				msg, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}

				if err != nil {
					emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: "recv: " + err.Error()})

					return
				}

				if _, err := fromPeer.Write(msg.GetData()); err != nil {
					emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: err.Error()})

					return
				}
			}

			peerTimes = append(peerTimes, time.Since(t0))

			if int64(len(fromPeer.Bytes())) != length {
				emit(result{
					Phase: "peer-fetch", Version: version, Outcome: "rejected",
					Detail: fmt.Sprintf("%s: peer streamed %d bytes for a %d-byte range",
						e.Build, len(fromPeer.Bytes()), length),
				})

				return
			}

			t1 := time.Now()

			localData, err := readRange(ctx, local, e.Payload, off, length, ft)
			if err != nil {
				emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: "local: " + err.Error()})

				return
			}

			localTimes = append(localTimes, time.Since(t1))

			// The peer's bytes must equal what the store holds: a peer that
			// serves different bytes is worse than no peer at all.
			if !bytes.Equal(fromPeer.Bytes(), localData) {
				emit(result{
					Phase: "peer-fetch", Version: version, Outcome: "misread",
					Detail: fmt.Sprintf("%s: peer bytes differ from the store at offset %d", e.Build, off),
				})

				return
			}

			if _, err := sum.Write(fromPeer.Bytes()); err != nil {
				emit(result{Phase: "peer-fetch", Version: version, Outcome: "error", Detail: err.Error()})

				return
			}

			bytesRead += length
		}

		got := hex.EncodeToString(sum.Sum(nil))
		if got != e.Checksum {
			emit(result{
				Phase: "peer-fetch", Version: version, Outcome: "misread",
				Detail: fmt.Sprintf("%s: peer bytes hash %s, manifest says %s", e.Build, got[:16], e.Checksum[:16]),
			})

			return
		}
	}

	slices.Sort(peerTimes)
	slices.Sort(localTimes)

	emit(result{
		Phase: "peer-fetch", Version: version, Outcome: "ok",
		Detail: fmt.Sprintf("read %d artifacts (%s) from the peer: p50 %s, p95 %s; same ranges from the store: p50 %s, p95 %s",
			len(m.Entries), humanBytes(bytesRead),
			percentile(peerTimes, 0.50).Round(time.Microsecond), percentile(peerTimes, 0.95).Round(time.Microsecond),
			percentile(localTimes, 0.50).Round(time.Microsecond), percentile(localTimes, 0.95).Round(time.Microsecond)),
		Objects: len(m.Entries), Bytes: bytesRead,
	})
}

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
		return strconv.FormatFloat(float64(n)/float64(1<<30), 'f', 1, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 1, 64) + " MiB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/float64(1<<10), 'f', 1, 64) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// bytesBuffer accumulates streamed bytes so they can be hashed or compared.
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)

	return len(p), nil
}

func (b *bytesBuffer) Bytes() []byte { return b.data }

var _ io.Writer = (*bytesBuffer)(nil)
