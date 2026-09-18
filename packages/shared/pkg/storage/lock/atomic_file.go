package lock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// ErrContentMismatch reports that a commit lost the first-writer-wins race to
// an existing file whose content differs from the bytes this writer produced.
var ErrContentMismatch = errors.New("immutable file already exists with different content")

// OpenOption configures OpenFile.
type OpenOption func(*openOptions)

type openOptions struct {
	// contentCheck makes Commit verify a lost race instead of trusting the
	// existing file blindly (see WithContentCheck).
	contentCheck bool
}

// WithContentCheck makes a losing writer hash the bytes it produced and
// compare them with the file that won the commit race. Identical content stays
// a successful dedup; different content is reported as ErrContentMismatch.
func WithContentCheck() OpenOption {
	return func(o *openOptions) { o.contentCheck = true }
}

// AtomicImmutableFile writes a file once. Bytes go to a private temp file and
// Commit publishes them atomically with a hard link, so the destination is
// either absent or complete. The destination is immutable and
// first-writer-wins: a writer that loses the race keeps the winner's bytes
// (normal cache dedup), and the lock keeps concurrent commits serialized.
// With WithContentCheck, a losing writer additionally verifies that the
// existing content matches what it wrote.
type AtomicImmutableFile struct {
	lockFile *os.File
	tempFile *os.File
	filename string

	// hasher is non-nil only when the caller asked for the content check; it
	// hashes the bytes as they are written so the comparison needs no second
	// copy of the payload.
	hasher hash.Hash

	closeOnce sync.Once
}

func (f *AtomicImmutableFile) Write(p []byte) (n int, err error) {
	n, err = f.tempFile.Write(p)
	if f.hasher != nil {
		f.hasher.Write(p[:n])
	}

	return n, err
}

var _ io.Writer = (*AtomicImmutableFile)(nil)

// OpenFile opens the immutable file for one writer: it takes the cache lock and
// creates a private temp file. Commit publishes it; Close drops it.
func OpenFile(ctx context.Context, filename string, opts ...OpenOption) (*AtomicImmutableFile, error) {
	options := openOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	lockFile, err := TryAcquireLock(ctx, filename)
	if err != nil {
		return nil, err
	}

	tempFilename := fmt.Sprintf("%s.temp.%s", filename, uuid.NewString())
	tempFile, err := os.OpenFile(tempFilename, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		utils.Cleanup(ctx, "failed to release lock",
			func() error { return ReleaseLock(ctx, lockFile) })

		return nil, fmt.Errorf("failed to open temp file: %w", err)
	}

	file := &AtomicImmutableFile{
		lockFile: lockFile,
		tempFile: tempFile,
		filename: filename,
	}
	if options.contentCheck {
		file.hasher = sha256.New()
	}

	return file, nil
}

// Commit publishes the written bytes. The destination is immutable: when it
// already exists the commit keeps it (first-writer-wins) and reports success —
// unless the caller asked for the content check and the existing bytes differ.
func (f *AtomicImmutableFile) Commit(ctx context.Context) error {
	return f.close(ctx, true)
}

// Close drops the temp file without publishing it.
func (f *AtomicImmutableFile) Close(ctx context.Context) error {
	return f.close(ctx, false)
}

func (f *AtomicImmutableFile) close(ctx context.Context, success bool) error {
	var err error

	f.closeOnce.Do(func() {
		var errs []error

		defer utils.Cleanup(ctx, "failed to unlock file", func() error {
			return ReleaseLock(ctx, f.lockFile)
		})

		// fsync before the link publishes the file: a crash must not leave a
		// visible cache entry whose contents were never flushed (REQ-C2).
		if success {
			if err = f.tempFile.Sync(); err != nil {
				errs = append(errs, fmt.Errorf("failed to sync temp file: %w", err))

				success = false
			}
		}

		if err = f.tempFile.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close temp file: %w", err))
		}

		if success {
			if err = utils.RenameOrDeleteFile(ctx, f.tempFile.Name(), f.filename); err != nil {
				if errors.Is(err, syscall.EEXIST) {
					// First-writer-wins: the existing immutable file stays. With
					// the content check, verify it holds the bytes we wrote.
					if checkErr := f.verifyExistingContent(ctx); checkErr != nil {
						errs = append(errs, checkErr)
					}
				} else {
					errs = append(errs, fmt.Errorf("failed to commit file: %w", err))
				}
			}
		} else {
			errs = append(errs, os.Remove(f.tempFile.Name()))
		}

		err = errors.Join(errs...)
	})

	return err
}

// verifyExistingContent compares the bytes this writer produced with the file
// that won the commit race. It is a no-op unless the caller enabled
// WithContentCheck: without it, losing the race is normal cache dedup and the
// existing file is trusted.
func (f *AtomicImmutableFile) verifyExistingContent(ctx context.Context) error {
	if f.hasher == nil {
		return nil
	}

	existing, err := os.Open(f.filename)
	if err != nil {
		return fmt.Errorf("failed to open existing immutable file %s: %w", f.filename, err)
	}
	defer utils.Cleanup(ctx, "failed to close existing immutable file", existing.Close)

	existingHash := sha256.New()
	if _, err := io.Copy(existingHash, existing); err != nil {
		return fmt.Errorf("failed to hash existing immutable file %s: %w", f.filename, err)
	}

	if !bytes.Equal(existingHash.Sum(nil), f.hasher.Sum(nil)) {
		return fmt.Errorf("%w: %s", ErrContentMismatch, f.filename)
	}

	return nil
}
