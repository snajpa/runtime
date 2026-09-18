package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type fsStorage struct {
	basePath  string
	uploadURL string // base URL for local upload endpoint (e.g. "http://localhost:5008")
	hmacKey   []byte // HMAC key for signing upload tokens
}

var _ StorageProvider = (*fsStorage)(nil)

type fsObject struct {
	path    string
	objType SeekableObjectType
}

var (
	_ Seekable    = (*fsObject)(nil)
	_ Blob        = (*fsObject)(nil)
	_ RangeOpener = (*fsObject)(nil)
)

func newFileSystemStorage(basePath, uploadBaseURL string, hmacKey []byte) *fsStorage {
	return &fsStorage{
		basePath:  basePath,
		uploadURL: uploadBaseURL,
		hmacKey:   hmacKey,
	}
}

func (s *fsStorage) DeleteObjectsWithPrefix(_ context.Context, prefix string) error {
	filePath, err := s.getPath(prefix)
	if err != nil {
		return err
	}

	return os.RemoveAll(filePath)
}

func (s *fsStorage) GetDetails() string {
	return fmt.Sprintf("[Local file storage, base path set to %s]", s.basePath)
}

// Capabilities implements CapabilityReporter. The filesystem provider deletes
// a whole prefix with one RemoveAll (no batching), can discard the temp file
// of an in-flight write, signs upload URLs only when the local upload
// endpoint is configured, and has neither custom metadata nor multipart
// uploads.
func (s *fsStorage) Capabilities() Capabilities {
	return Capabilities{
		Name:            "fs",
		AbortUpload:     true,
		SignedUploadURL: s.uploadURL != "" && len(s.hmacKey) > 0,
	}
}

func (s *fsStorage) UploadSignedURL(_ context.Context, path string, ttl time.Duration) (UploadURL, error) {
	if s.uploadURL == "" || s.hmacKey == nil {
		return UploadURL{}, errors.New("file system storage does not support signed URLs (no local upload endpoint configured)")
	}

	expiresSec := time.Now().Add(ttl).Unix()
	token := ComputeUploadHMAC(s.hmacKey, path, expiresSec)

	u := fmt.Sprintf("%s/upload?path=%s&expires=%d&token=%s",
		s.uploadURL, url.QueryEscape(path), expiresSec, url.QueryEscape(token))

	return UploadURL{URL: u}, nil
}

func (s *fsStorage) OpenSeekable(_ context.Context, path string) (Seekable, error) {
	fullPath, err := s.getPath(path)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	objType, _ := seekableObjectType(path)

	return &fsObject{
		path:    fullPath,
		objType: objType,
	}, nil
}

func (s *fsStorage) OpenBlob(_ context.Context, path string) (Blob, error) {
	fullPath, err := s.getPath(path)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	return &fsObject{
		path: fullPath,
	}, nil
}

// getPath resolves an object path against the provider's base directory. Every
// call site runs the shared containment check, so a traversal-shaped or empty
// name never reaches the filesystem (the prefix delete is irreversible).
func (s *fsStorage) getPath(path string) (string, error) {
	return ContainedPath(s.basePath, path)
}

func (o *fsObject) WriteTo(ctx context.Context, dst io.Writer) (n int64, err error) {
	start := time.Now()
	defer func() { RecordReadBlob(ctx, time.Since(start), n, o.path, SourceFS, err) }()

	handle, err := o.openRead()
	if err != nil {
		return 0, err
	}

	defer handle.Close()

	n, err = io.Copy(dst, handle)

	return n, err
}

func (o *fsObject) Put(_ context.Context, data []byte, _ ...PutOption) error {
	return o.atomicWriteFile(0o644, func(w io.Writer) error {
		_, err := io.Copy(w, bytes.NewReader(data))

		return err
	})
}

func (o *fsObject) StoreFile(ctx context.Context, path string, opts ...PutOption) (*FullFrameTable, [32]byte, error) {
	putOpts := ApplyPutOptions(opts)
	cfg := CompressConfigFromOpts(putOpts)
	if cfg.IsCompressionEnabled() {
		ft, checksum, err := o.storeFileCompressed(ctx, path, cfg, putOpts.FrameSink)
		if err == nil {
			t := ft.Table()
			logger.L().Debug(ctx, "Stored file to filesystem",
				zap.String("object", o.path),
				zap.String("source", path),
				zap.Int64("size_uncompressed", t.UncompressedSize()),
				zap.Int64("size_compressed", t.CompressedSize()),
				zap.String("compression", cfg.CompressionType().String()),
				zap.Int("frames", t.NumFrames()),
			)
		}

		return ft, checksum, err
	}

	r, err := os.Open(path)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer r.Close()

	var n int64
	err = o.atomicWriteFile(0o644, func(w io.Writer) error {
		var copyErr error
		n, copyErr = io.Copy(w, r)

		return copyErr
	})
	if err == nil {
		logger.L().Debug(ctx, "Stored file to filesystem",
			zap.String("object", o.path),
			zap.String("source", path),
			zap.Int64("size_uncompressed", n),
			zap.String("compression", "none"),
		)
	}

	return nil, [32]byte{}, err
}

func (o *fsObject) storeFileCompressed(ctx context.Context, localPath string, cfg CompressConfig, sink FrameSink) (*FullFrameTable, [32]byte, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("failed to open local file %s: %w", localPath, err)
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("failed to stat local file %s: %w", localPath, err)
	}

	uploader := &fsPartUploader{fullPath: o.path}

	const noConcurrencyForMemUploader = 1
	ft, checksum, err := compressStream(ctx, file, cfg, uploader, noConcurrencyForMemUploader, sink)
	if err != nil {
		return nil, [32]byte{}, err
	}

	// Sidecar is written only after compressStream succeeds so a failure (cancel,
	// partial read, compress error) doesn't leave Size() reporting the new size
	// against the unchanged data file.
	sidecarPath := SizeSidecar(o.path)
	if writeErr := writeFileAtomic(sidecarPath, 0o644, func(w io.Writer) error {
		_, err := w.Write([]byte(strconv.FormatInt(fi.Size(), 10)))

		return err
	}); writeErr != nil {
		return nil, [32]byte{}, fmt.Errorf("failed to write uncompressed-size sidecar for %s: %w", o.path, writeErr)
	}

	return ft, checksum, nil
}

func (o *fsObject) openRangeReader(_ context.Context, off, length int64) (RangeReader, error) {
	f, err := o.openRead()
	if err != nil {
		return nil, err
	}

	return newSectionReader(f, off, length), nil
}

func (o *fsObject) Exists(_ context.Context) (bool, error) {
	_, err := os.Stat(o.path)
	if os.IsNotExist(err) {
		return false, nil
	}

	return err == nil, err
}

func (o *fsObject) Size(ctx context.Context) (_ int64, err error) {
	start := time.Now()
	defer func() { RecordReadSize(ctx, time.Since(start), o.objType, SourceFS, err) }()

	handle, err := o.openRead()
	if err != nil {
		return 0, err
	}
	defer handle.Close()

	fileInfo, err := handle.Stat()
	if err != nil {
		return 0, err
	}

	// Check for .uncompressed-size sidecar file
	sidecarPath := SizeSidecar(o.path)
	if sidecarData, sidecarErr := os.ReadFile(sidecarPath); sidecarErr == nil {
		if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(string(sidecarData)), 10, 64); parseErr == nil {
			return parsed, nil
		}
	}

	return fileInfo.Size(), nil
}

func (o *fsObject) Delete(_ context.Context) error {
	return os.Remove(o.path)
}

func ComputeUploadHMAC(key []byte, path string, expires int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(path))
	mac.Write([]byte{0}) // delimiter to prevent path/expires boundary ambiguity
	mac.Write([]byte(strconv.FormatInt(expires, 10)))

	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateUploadToken validates an HMAC token for a local upload URL.
// Exported so that the upload handler in the orchestrator can use it.
func ValidateUploadToken(key []byte, path string, expires int64, token string) bool {
	if time.Now().Unix() > expires {
		return false
	}

	expected := ComputeUploadHMAC(key, path, expires)

	return hmac.Equal([]byte(expected), []byte(token))
}

// openRead opens the object read-only. Reads never create files and never
// require write permission on the object or its directory.
func (o *fsObject) openRead() (*os.File, error) {
	handle, err := os.Open(o.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotExist
		}

		return nil, err
	}

	info, err := handle.Stat()
	if err != nil {
		handle.Close()

		return nil, err
	}
	if info.IsDir() {
		handle.Close()

		return nil, fmt.Errorf("path %s is a directory", o.path)
	}

	return handle, nil
}

// writeFileAtomic writes path through a same-directory temp file and renames
// it into place, so readers never observe a partial object and a rewrite
// always replaces the previous content (no stale tail). The file is fsynced
// before the rename; the parent directory entry is not fsynced.
func writeFileAtomic(path string, perm os.FileMode, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := write(tmp); err != nil {
		cleanup()

		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()

		return fmt.Errorf("failed to sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("failed to close %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("failed to chmod %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("failed to rename temp file into %s: %w", path, err)
	}

	return nil
}

// atomicWriteFile writes this object atomically with the given permissions.
func (o *fsObject) atomicWriteFile(perm os.FileMode, write func(io.Writer) error) error {
	return writeFileAtomic(o.path, perm, write)
}

// fsPartUploader implements partUploader for local filesystem.
// Embeds memPartUploader for concurrent-safe part collection,
// then writes atomically on Complete.
type fsPartUploader struct {
	memPartUploader

	fullPath string
}

func (u *fsPartUploader) Complete(_ context.Context) error {
	data := u.Assemble()

	return writeFileAtomic(u.fullPath, 0o644, func(w io.Writer) error {
		_, err := w.Write(data)

		return err
	})
}

// ProviderName overrides the embedded in-memory uploader: parts are staged in
// memory and written atomically on Complete, so a failed upload leaves no file
// behind.
func (u *fsPartUploader) ProviderName() string { return "fs" }

func (o *fsObject) OpenRangeReader(ctx context.Context, offsetU int64, length int64, frameTable *FrameTable) (_ RangeReader, _ Source, err error) {
	start := time.Now()
	defer func() { RecordReadOpen(ctx, time.Since(start), o.objType, SourceFS, frameTable.CompressionType(), err) }()

	if frameTable.IsCompressed() {
		r, err := frameTable.LocateCompressed(offsetU)
		if err != nil {
			return nil, SourceFS, fmt.Errorf("get frame for offset %d, FS:%s: %w", offsetU, o.path, err)
		}

		raw, err := o.openRangeReader(ctx, r.Offset, int64(r.Length))
		if err != nil {
			return nil, SourceFS, err
		}

		dec, err := NewDecompressReader(raw, frameTable.CompressionType(), SourceFS, o.objType)
		if err != nil {
			raw.Close(ctx)

			return nil, SourceFS, err
		}

		return dec, SourceFS, nil
	}

	raw, err := o.openRangeReader(ctx, offsetU, length)
	if err != nil {
		return nil, SourceFS, err
	}

	return raw, SourceFS, nil
}
