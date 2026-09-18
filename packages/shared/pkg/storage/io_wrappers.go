package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

var (
	_ io.Reader   = (*meteredReader)(nil)
	_ RangeReader = (*sectionReader)(nil)
	_ RangeReader = (*spanReader)(nil)
	_ RangeReader = (*rangeReader)(nil)
	_ RangeReader = (*captureReader)(nil)
)

// readMeter accumulates bytes read and time spent reading. Embed it in a reader
// that meters its own source inline: call observe() in Read, stats() in Close.
// Unlike meteredReader it does not wrap anything, so there is no ambiguity about
// which object to read from.
type readMeter struct {
	bytes int64
	read  time.Duration
}

func (m *readMeter) observe(n int, since time.Time) {
	m.bytes += int64(n)
	m.read += time.Since(since)
}

// stats reports the meter as a ReadStats. Stored and delivered counts are equal —
// these readers don't decompress (decompressReader builds its own).
func (m *readMeter) stats() *ReadStats {
	return &ReadStats{
		StoredBytes:    m.bytes,
		DeliveredBytes: m.bytes,
		Read:           m.read,
	}
}

// rangeReader adapts an io.ReadCloser into a self-metering RangeReader.
type rangeReader struct {
	readMeter

	rc io.ReadCloser
}

func NewRangeReader(rc io.ReadCloser) RangeReader { return &rangeReader{rc: rc} }

func (r *rangeReader) Read(p []byte) (int, error) {
	t0 := time.Now()
	n, err := r.rc.Read(p)
	r.observe(n, t0)

	return n, err
}

func (r *rangeReader) Close(context.Context) (*ReadStats, error) {
	return r.stats(), r.rc.Close()
}

type sectionReader struct {
	readMeter

	sr   *io.SectionReader
	file *os.File
}

func newSectionReader(f *os.File, off, length int64) *sectionReader {
	return &sectionReader{
		sr:   io.NewSectionReader(f, off, length),
		file: f,
	}
}

func (r *sectionReader) Read(p []byte) (int, error) {
	t0 := time.Now()
	n, err := r.sr.Read(p)
	r.observe(n, t0)

	return n, err
}

func (r *sectionReader) Close(context.Context) (*ReadStats, error) {
	return r.stats(), r.file.Close()
}

// meteredReader meters reads pulled through it by a downstream consumer (a
// decoder in the decompress pipeline), where inline metering isn't possible
// because this reader isn't the one calling the source's Read.
type meteredReader struct {
	readMeter

	inner io.Reader
}

func (m *meteredReader) Read(p []byte) (int, error) {
	t0 := time.Now()
	n, err := m.inner.Read(p)
	m.observe(n, t0)

	return n, err
}

// capturedBytes is one captureReader payload: the captured bytes plus the
// release that returns the backing buffer to the pool. onClose receives it and
// must call Release once the bytes are no longer read — including when the
// writeback is skipped, dropped, or fails, and before returning when it does
// not keep the payload. Release is safe to call more than once.
type capturedBytes struct {
	data    []byte
	release func()
}

// Bytes returns the captured payload; it is only valid until Release is called.
func (c capturedBytes) Bytes() []byte { return c.data }

// Release returns the backing buffer to the pool.
func (c capturedBytes) Release() {
	if c.release != nil {
		c.release()
	}
}

// maxPooledCaptureSize bounds what the capture pool keeps. Captures can be as
// large as a memory chunk (4 MiB); pooling beyond that only holds memory the
// GC would otherwise reclaim.
const maxPooledCaptureSize = MemoryChunkSize

// captureBufPool pools the capture buffers of the read path's cache writebacks:
// a cache miss allocates one capture buffer per read, and chunk-sized reads hit
// the same size class over and over (REQ-D3).
var captureBufPool = newBufferPool()

// captureReader tees every read byte into a buffer and hands the captured
// payload to onClose on Close. Used by the cache writeback paths.
// drainOnClose=true reads inner to EOF on Close even if the caller above hasn't
// consumed everything — the compressed cache needs the full frame regardless
// of how many bytes the decoder happened to demand.
type captureReader struct {
	inner        RangeReader
	buf          []byte
	free         func() // returns the pooled backing array; nil once detached
	onClose      func(ctx context.Context, captured capturedBytes)
	drainOnClose bool
}

// newCaptureReader tees the reads of inner into a pooled capture buffer of
// capHint (the expected payload). A hint outside the pool's bound falls back to
// a plain allocation, so a read cannot park an arbitrarily large buffer in the
// pool.
func newCaptureReader(inner RangeReader, capHint int, drainOnClose bool, onClose func(context.Context, capturedBytes)) *captureReader {
	r := &captureReader{
		inner:        inner,
		onClose:      onClose,
		drainOnClose: drainOnClose,
	}

	switch {
	case capHint <= 0:
		// No hint: capture into a plain buffer.
	case capHint <= maxPooledCaptureSize:
		pooled := captureBufPool.Get(capHint)
		r.buf = pooled.Bytes()[:0]
		r.free = pooled.Free
	default:
		r.buf = make([]byte, 0, capHint)
	}

	return r
}

func (r *captureReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.capture(p[:n])
	}

	return n, err
}

// capture appends to the capture buffer, detaching from the pooled array when
// the payload outgrows it: the pooled array cannot go back to the pool while
// the payload lives in it, so the reader continues in a buffer of its own.
func (r *captureReader) capture(p []byte) {
	if r.free != nil && len(r.buf)+len(p) > cap(r.buf) {
		grown := make([]byte, len(r.buf), len(r.buf)+len(p))
		copy(grown, r.buf)
		r.buf = grown
		r.free()
		r.free = nil
	}

	r.buf = append(r.buf, p...)
}

func (r *captureReader) Close(ctx context.Context) (*ReadStats, error) {
	if r.drainOnClose {
		_, _ = io.Copy(io.Discard, r)
	}

	stats, err := r.inner.Close(ctx)

	onClose := r.onClose
	r.onClose = nil
	if onClose == nil {
		return stats, err
	}

	if err != nil {
		// The consumer never receives the payload: return the buffer.
		r.releaseBuffer()

		return stats, err
	}

	onClose(ctx, capturedBytes{data: r.buf, release: r.takeRelease()})

	return stats, err
}

// takeRelease hands ownership of the pooled buffer to the captured payload.
// The returned func is safe to call more than once.
func (r *captureReader) takeRelease() func() {
	free := r.free
	r.free = nil
	if free == nil {
		return func() {}
	}

	var once sync.Once

	return func() { once.Do(free) }
}

// releaseBuffer returns the pooled buffer when no payload was handed out.
func (r *captureReader) releaseBuffer() {
	if r.free == nil {
		return
	}

	r.free()
	r.free = nil
}

// spanReader ends a trace span on Close, recording the close error or the first
// non-EOF read error against it. Stats pass through from inner, which (like every
// RangeReader) self-reports them.
type spanReader struct {
	inner   RangeReader
	span    trace.Span
	readErr error
}

func newSpanReader(inner RangeReader, span trace.Span) *spanReader {
	return &spanReader{inner: inner, span: span}
}

func (r *spanReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.readErr = err
	}

	return n, err
}

func (r *spanReader) Close(ctx context.Context) (*ReadStats, error) {
	stats, closeErr := r.inner.Close(ctx)

	if closeErr != nil {
		recordError(r.span, closeErr)
	} else if r.readErr != nil {
		recordError(r.span, r.readErr)
	}
	r.span.End()

	return stats, closeErr
}

// sliceReaderAt is a stateless io.ReaderAt over multiple byte slices; see
// newMultiSliceReader.
type sliceReaderAt struct {
	slices [][]byte
}

func (r sliceReaderAt) ReadAt(p []byte, off int64) (int, error) {
	// io.ReaderAt requires a non-negative offset; return an error rather than
	// panicking on the slice reslice below if used directly (io.SectionReader,
	// the only current caller, already guards this).
	if off < 0 {
		return 0, errors.New("storage: sliceReaderAt.ReadAt: negative offset")
	}

	var n int
	for _, s := range r.slices {
		if off >= int64(len(s)) {
			off -= int64(len(s))

			continue
		}

		n += copy(p[n:], s[off:])
		off = 0
		if n == len(p) {
			return n, nil
		}
	}

	return n, io.EOF
}

// multiSliceReader is a seekable reader over multiple byte slices. Len
// reports the unread byte count so HTTP clients (retryablehttp's LenReader)
// send an explicit Content-Length instead of chunked transfer encoding.
type multiSliceReader struct {
	*io.SectionReader
}

// Len returns the number of unread bytes, mirroring bytes.Reader.Len.
func (r *multiSliceReader) Len() int {
	cur, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}

	return int(r.Size() - cur)
}

// newMultiSliceReader streams multiple byte slices as one seekable body
// without concatenating them. Used as the multipart-part request body by both
// the GCP XML uploader (recreated per retry via ReaderFunc) and the AWS part
// uploader, where the SDK seeks to compute the payload hash/length and to
// rewind on retries.
func newMultiSliceReader(slices [][]byte) *multiSliceReader {
	var size int64
	for _, s := range slices {
		size += int64(len(s))
	}

	return &multiSliceReader{io.NewSectionReader(sliceReaderAt{slices: slices}, 0, size)}
}

// fileSectionReader streams a section of a file as a seekable request body
// without buffering it. The multipart file-upload path recreates it per retry
// via ReaderFunc; Len gives retryablehttp the Content-Length so parts are not
// sent chunked (S3-compatible XML backends reject chunked PUTs with 411).
type fileSectionReader struct {
	*io.SectionReader
}

func newFileSectionReader(f *os.File, off, length int64) *fileSectionReader {
	return &fileSectionReader{io.NewSectionReader(f, off, length)}
}

// Len returns the number of unread bytes, mirroring bytes.Reader.Len.
func (r *fileSectionReader) Len() int {
	cur, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}

	return int(r.Size() - cur)
}

// idleDeadlineReader cancels a streaming transfer's context when the wrapped
// reads stop making progress: every read that returns data extends the
// deadline, so a large-but-healthy transfer is never capped while a stalled
// one fails promptly (REQ-B2). It is the streaming counterpart of the
// read-path idle timeout.
type idleDeadlineReader struct {
	io.ReadCloser

	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
}

func newIdleDeadlineReader(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleDeadlineReader {
	r := &idleDeadlineReader{
		ReadCloser: body,
		timeout:    timeout,
		cancel:     cancel,
	}
	r.timer = time.AfterFunc(timeout, cancel)

	return r
}

func (r *idleDeadlineReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	switch {
	case err != nil:
		r.timer.Stop()
	case n > 0:
		r.timer.Reset(r.timeout)
	}

	return n, err
}

// stop halts the idle deadline; callers defer it so a completed transfer can
// never cancel its own context afterwards.
func (r *idleDeadlineReader) stop() {
	r.timer.Stop()
}
