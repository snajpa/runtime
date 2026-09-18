package storage

// Shared testcontainer scaffolding for the storage server-backed tests
// (MinIO / fake-gcs-server). Split out from #3113 so the GCS server tests and
// the AWS server tests can share it.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The container-backed storage tests run against an S3-compatible object
// store. Silo (pgsty/silo, the maintained MinIO fork) is the default backend;
// the pinned MinIO image stays selectable for one release cycle through
// E2B_STORAGE_TEST_BACKEND=minio (S-53). Both implement the S3/GCS XML
// multipart dialect the tests rely on; TestObjectStoreBackendDialect pins the
// dialect assumptions explicitly so a future swap cannot silently relax them.
const (
	siloImage  = "pgsty/silo:latest"
	minioImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z"

	backendEnv = "E2B_STORAGE_TEST_BACKEND"
)

// objectStoreBackendSpec is one selectable container image for the
// container-backed tests: image, command, environment and health check are
// the pieces that differ between S3-compatible servers.
type objectStoreBackendSpec struct {
	name       string
	image      string
	cmd        []string
	env        map[string]string
	healthPath string
}

var objectStoreBackends = map[string]objectStoreBackendSpec{
	"silo": {
		name:       "silo",
		image:      siloImage,
		cmd:        []string{"server", "/data"},
		env:        map[string]string{"MINIO_ROOT_USER": "minioadmin", "MINIO_ROOT_PASSWORD": "minioadmin"},
		healthPath: "/minio/health/live",
	},
	"minio": {
		name:       "minio",
		image:      minioImage,
		cmd:        []string{"server", "/data"},
		env:        map[string]string{"MINIO_ROOT_USER": "minioadmin", "MINIO_ROOT_PASSWORD": "minioadmin"},
		healthPath: "/minio/health/live",
	},
}

// objectStoreBackend returns the backend selected by E2B_STORAGE_TEST_BACKEND
// (default: silo).
func objectStoreBackend(t *testing.T) objectStoreBackendSpec {
	t.Helper()

	name := strings.ToLower(os.Getenv(backendEnv))
	if name == "" {
		return objectStoreBackends["silo"]
	}

	spec, ok := objectStoreBackends[name]
	require.Truef(t, ok, "unknown %s %q: want one of silo, minio", backendEnv, name)

	return spec
}

// s3TestBackend describes where the tests run: a real AWS bucket (endpoint
// empty) or an S3-compatible container endpoint.
type s3TestBackend struct {
	bucket   string
	endpoint string
}

// startObjectStoreBackend starts a per-test object-store container and
// creates a bucket in it (same pattern as redis_utils.SetupInstance — Docker
// required, torn down via t.Cleanup). Also used by the GCS XML multipart
// tests, which need the S3/GCS XML multipart dialect.
func startObjectStoreBackend(t *testing.T) *s3TestBackend {
	t.Helper()

	spec := objectStoreBackend(t)

	container, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        spec.image,
			Cmd:          spec.cmd,
			Env:          spec.env,
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForHTTP(spec.healthPath).WithPort("9000/tcp"),
		},
		Started: true,
	})
	require.NoError(t, err, "start %s container", spec.name)

	t.Cleanup(func() {
		if err := container.Terminate(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("cleanup: failed to terminate %s container: %v", spec.name, err)
		}
	})

	host, err := container.Host(t.Context())
	require.NoError(t, err)
	port, err := container.MappedPort(t.Context(), "9000")
	require.NoError(t, err)

	backend := &s3TestBackend{
		bucket:   "s3-test-bucket",
		endpoint: fmt.Sprintf("http://%s:%s", host, port.Port()),
	}

	_, err = backend.newClient(t, nil).CreateBucket(t.Context(),
		&s3.CreateBucketInput{Bucket: aws.String(backend.bucket)})
	require.NoError(t, err, "create bucket")

	return backend
}

// newClient builds an S3 client for the backend. httpClient is optional and
// lets tests install a fault-injecting transport; optFns lets them tune
// client options such as the retryer.
func (b *s3TestBackend) newClient(t *testing.T, httpClient *http.Client, optFns ...func(*s3.Options)) *s3.Client {
	t.Helper()

	if b.endpoint != "" {
		cfg := aws.Config{
			// Credentials match objectStoreBackends' MINIO_ROOT_USER/PASSWORD.
			Credentials: credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
			Region:      "us-east-1",
		}
		if httpClient != nil {
			cfg.HTTPClient = httpClient
		}

		return s3.NewFromConfig(cfg, append([]func(*s3.Options){func(o *s3.Options) {
			o.BaseEndpoint = aws.String(b.endpoint)
			o.UsePathStyle = true
		}}, optFns...)...)
	}

	var opts []func(*config.LoadOptions) error
	if httpClient != nil {
		opts = append(opts, config.WithHTTPClient(httpClient))
	}
	cfg, err := config.LoadDefaultConfig(t.Context(), opts...)
	require.NoError(t, err)

	return s3.NewFromConfig(cfg, optFns...)
}

func testKey(name string) string {
	return fmt.Sprintf("s3-test/%d/%s", time.Now().UnixNano(), name)
}

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "input")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	return path
}

func testCompressConfig() CompressConfig {
	return CompressConfig{Enabled: true, Type: CompressionLZ4.String(), FrameSizeKB: 1, MinPartSizeMB: 1, FrameEncodeWorkers: 1}
}

// countingReadCloser counts bytes read through it and reports the total to
// onClose when the body is closed. The count is read in Close (after the
// transport is done with the body) rather than after RoundTrip returns, which
// would race the transport's still-running body reads.
type countingReadCloser struct {
	rc      io.ReadCloser
	n       atomic.Int64
	onClose func(int64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))

	return n, err
}

func (c *countingReadCloser) Close() error {
	err := c.rc.Close()
	if c.onClose != nil {
		c.onClose(c.n.Load())
	}

	return err
}

// faultInjectingTransport sits below the AWS SDK (after signing): the first
// attempt of every distinct S3 request consumes the full request body and
// gets a synthetic 500 InternalError, forcing the SDK's real retry machinery
// — including body rewind via Seek — before the retry passes through to real
// S3. Per-part body sizes are recorded per attempt so a broken rewind (short
// or empty re-send) is directly observable.
type faultInjectingTransport struct {
	inner http.RoundTripper

	mu            sync.Mutex
	seen          map[string]bool
	injected      int
	partBodySizes map[string][]int64 // partNumber -> body bytes per attempt
}

func newFaultInjectingTransport() *faultInjectingTransport {
	return &faultInjectingTransport{
		inner:         http.DefaultTransport,
		seen:          make(map[string]bool),
		partBodySizes: make(map[string][]int64),
	}
}

func (f *faultInjectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Distinct request identity: retries repeat all of these exactly.
	key := req.Method + " " + req.URL.Path + "?" + req.URL.RawQuery + " range=" + req.Header.Get("Range")
	partNum := req.URL.Query().Get("partNumber")

	f.mu.Lock()
	first := !f.seen[key]
	f.seen[key] = true
	f.mu.Unlock()

	if first {
		var consumed int64
		if req.Body != nil {
			consumed, _ = io.Copy(io.Discard, req.Body)
			req.Body.Close()
		}

		f.mu.Lock()
		f.injected++
		if partNum != "" {
			f.partBodySizes[partNum] = append(f.partBodySizes[partNum], consumed)
		}
		f.mu.Unlock()

		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{"Content-Type": []string{"application/xml"}},
			Body: io.NopCloser(strings.NewReader(
				`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message>injected fault</Message></Error>`)),
			Request: req,
		}, nil
	}

	if partNum != "" && req.Body != nil {
		counter := &countingReadCloser{
			rc: req.Body,
			// Record on Close, not after RoundTrip returns: the transport may
			// still be reading/closing the body when RoundTrip returns.
			onClose: func(n int64) {
				f.mu.Lock()
				f.partBodySizes[partNum] = append(f.partBodySizes[partNum], n)
				f.mu.Unlock()
			},
		}
		clone := req.Clone(req.Context())
		clone.Body = counter

		return f.inner.RoundTrip(clone)
	}

	return f.inner.RoundTrip(req)
}

// TestObjectStoreBackendDialect pins the server-side dialect assumptions the
// storage tests depend on so a backend swap (S-53) cannot silently relax
// them: out-of-order part uploads must be accepted and reassembled by part
// number, and a chunked upload without Content-Length must be rejected with
// 411.
func TestObjectStoreBackendDialect(t *testing.T) {
	t.Parallel()

	backend := startObjectStoreBackend(t)
	client := backend.newClient(t, nil)

	t.Run("out-of-order parts", func(t *testing.T) {
		t.Parallel()

		key := testKey("dialect-out-of-order")
		obj := backend.object(t, client, key)

		// Non-final parts must be >= 5 MiB; the final part may be tiny.
		part1 := bytes.Repeat([]byte{0xA1}, 5*megabyte+1)
		part2 := []byte("dialect-final-part")

		created, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
			Bucket: aws.String(backend.bucket),
			Key:    aws.String(key),
		})
		require.NoError(t, err)

		uploadID := created.UploadId

		// Upload the final part first: the dialect accepts part numbers in any
		// upload order and reassembles by part number on complete.
		up2, err := client.UploadPart(t.Context(), &s3.UploadPartInput{
			Bucket:     aws.String(backend.bucket),
			Key:        aws.String(key),
			UploadId:   uploadID,
			PartNumber: aws.Int32(2),
			Body:       bytes.NewReader(part2),
		})
		require.NoError(t, err, "backend must accept the final part before earlier parts")

		up1, err := client.UploadPart(t.Context(), &s3.UploadPartInput{
			Bucket:     aws.String(backend.bucket),
			Key:        aws.String(key),
			UploadId:   uploadID,
			PartNumber: aws.Int32(1),
			Body:       bytes.NewReader(part1),
		})
		require.NoError(t, err)

		_, err = client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
			Bucket:   aws.String(backend.bucket),
			Key:      aws.String(key),
			UploadId: uploadID,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{ETag: up1.ETag, PartNumber: aws.Int32(1)},
				{ETag: up2.ETag, PartNumber: aws.Int32(2)},
			}},
		})
		require.NoError(t, err)

		want := bytes.Join([][]byte{part1, part2}, nil)

		var got bytes.Buffer

		_, err = obj.WriteTo(t.Context(), &got)
		require.NoError(t, err)
		require.Equal(t, want, got.Bytes(),
			"parts must reassemble in part-number order, not upload order")
	})

	t.Run("chunked upload without content-length", func(t *testing.T) {
		t.Parallel()

		key := testKey("dialect-chunked")

		signed, err := s3.NewPresignClient(client).PresignPutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(backend.bucket),
			Key:    aws.String(key),
		})
		require.NoError(t, err)

		body := "dialect-chunked-body"

		// Hide the concrete reader from http.NewRequest so no Content-Length is
		// derived, and force chunked transfer encoding explicitly.
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, signed.URL,
			io.NopCloser(io.LimitReader(strings.NewReader(body), int64(len(body)))))
		require.NoError(t, err)
		req.TransferEncoding = []string{"chunked"}

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusLengthRequired, resp.StatusCode,
			"backend must reject a chunked upload without Content-Length with 411: %s", respBody)
	})
}
