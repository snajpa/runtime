package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	// defaultLockTTL is the time after which a lock file is considered stale
	// (its owner either finished without releasing or died). Takeover markers
	// do not use it: marker ownership is kernel-tracked (flock).
	defaultLockTTL = 10 * time.Second
	// lockFileMode is the file mode for lock files. The lock is advisory and
	// holds only the owner token.
	lockFileMode = 0o644
	// staleTakeoverAttempts bounds the stat/takeover/create retries when
	// several writers race to take over the same stale lock.
	staleTakeoverAttempts = 3
)

var (
	ErrLockAlreadyHeld = errors.New("lock is already held by another process")

	// errLockChanged is internal to TryAcquireLock: the path changed between
	// two observations of one attempt, so the attempt is retried from the top.
	errLockChanged = errors.New("lock path changed during the attempt")
)

// getLockFilePath generates a lock file path from a key
func getLockFilePath(path string) string {
	return path + ".lock"
}

// getTakeoverMarkerPath is the path of the marker that serializes changes to
// one lock file: creating it and taking over a stale lock.
func getTakeoverMarkerPath(lockPath string) string {
	return lockPath + ".takeover"
}

// TryAcquireLock attempts to acquire the advisory cache lock for path.
//
// Contention is reported as ErrLockAlreadyHeld and never blocks: callers
// either retry with backoff (the cache-fill writebacks do, see
// retryContendedLock) or treat it as normal cache dedup and skip the write.
//
// A lock whose mtime is older than defaultLockTTL is stale and may be taken
// over. Every change to the lock path — creating the lock, taking over a stale
// one, releasing one — happens while holding the lock's takeover marker (the
// path's ".takeover" sibling), and the path is re-read once the marker is
// held. Two properties follow from that:
//
//   - A writer cannot create a lock in a path a takeover has just emptied,
//     because creation and takeover exclude each other through the marker.
//   - A writer whose observation of a stale lock is outdated re-reads the path
//     under the marker and finds the fresh lock another writer created there,
//     so a fresh lock is never renamed away from its owner: the stale
//     observation simply loses.
//
// Marker ownership is kernel-tracked: the holder is the process holding an
// exclusive flock on the marker file. A live holder is never displaced — an
// aged marker with a live holder is contention, not a takeover candidate — a
// holder that died is recovered immediately (the kernel drops its lock), and
// the marker a holder releases is unlinked only by that holder, and only while
// it is still the file that was locked.
//
// The owner token written into the lock file keeps ReleaseLock from deleting a
// lock that was taken over in the meantime; the release itself runs under the
// marker as well.
func TryAcquireLock(ctx context.Context, path string) (*os.File, error) {
	lockPath := getLockFilePath(path)

	var err error

	for range staleTakeoverAttempts {
		var file *os.File

		file, err = tryAcquireLock(ctx, lockPath)
		if err == nil {
			return file, nil
		}

		if !errors.Is(err, errLockChanged) {
			return nil, err
		}
	}

	if err == nil {
		err = ErrLockAlreadyHeld
	}

	return nil, err
}

// tryAcquireLock makes one attempt at acquiring the lock: it observes the path,
// takes the takeover marker, re-reads the path under it, and then creates the
// lock, takes over the stale lock it observed, or reports what changed in
// between.
func tryAcquireLock(ctx context.Context, lockPath string) (*os.File, error) {
	stale, staleErr := os.Stat(lockPath)
	if staleErr == nil && time.Since(stale.ModTime()) <= defaultLockTTL {
		// Fresh lock: held by another writer.
		return nil, ErrLockAlreadyHeld
	}

	if staleErr != nil && !os.IsNotExist(staleErr) {
		return nil, fmt.Errorf("failed to stat lock file %s: %w", lockPath, staleErr)
	}

	marker, err := lockTakeoverMarker(ctx, lockPath)
	if err != nil {
		return nil, err
	}

	defer releaseTakeoverMarker(ctx, marker)

	current, err := os.Stat(lockPath)
	switch {
	case os.IsNotExist(err):
		// The path is free: create the lock. The marker keeps every other
		// writer out of the path while this runs.
		return createLockFileOrContention(lockPath)
	case err != nil:
		return nil, fmt.Errorf("failed to stat lock file %s: %w", lockPath, err)
	case time.Since(current.ModTime()) <= defaultLockTTL:
		// A fresh lock appeared between the observation and the marker: the
		// writer that created it keeps it.
		return nil, ErrLockAlreadyHeld
	case staleErr != nil || !os.SameFile(stale, current):
		// A different, still stale file is there now (another writer's takeover
		// or cleanup landed in between): observe again.
		return nil, errLockChanged
	}

	logger.L().Debug(ctx, "Found stale lock file, taking it over",
		zap.String("path", lockPath),
		zap.Duration("age", time.Since(current.ModTime())))

	// The stale lock this attempt observed is still at the path, and no other
	// writer can touch the path while the marker is held, so this rename moves
	// exactly that file.
	tombstone := fmt.Sprintf("%s.stale.%s", lockPath, uuid.NewString())

	if err := os.Rename(lockPath, tombstone); err != nil {
		if os.IsNotExist(err) {
			// Gone between the stat and the rename: observe again.
			return nil, errLockChanged
		}

		return nil, fmt.Errorf("failed to take over stale lock %s: %w", lockPath, err)
	}

	if err := os.Remove(tombstone); err != nil && !os.IsNotExist(err) {
		logger.L().Warn(ctx, "Failed to remove stale lock tombstone",
			zap.String("path", tombstone),
			zap.Error(err))
	}

	return createLockFileOrContention(lockPath)
}

// lockTakeoverMarker creates or adopts the marker that serializes changes to
// one lock path. Ownership is kernel-tracked: the holder is the process
// holding an exclusive flock on the marker file, so a live holder is never
// displaced — however old the marker looks — and a holder that died is
// replaced at once, because the kernel drops its lock. An existing marker is
// adopted only after re-verifying, under that lock, that the path still names
// the file that was locked.
func lockTakeoverMarker(ctx context.Context, lockPath string) (*os.File, error) {
	markerPath := getTakeoverMarkerPath(lockPath)

	for range staleTakeoverAttempts {
		created := false

		file, err := createExclusive(markerPath)

		switch {
		case err == nil:
			created = true
		case os.IsExist(err):
			file, err = os.OpenFile(markerPath, os.O_RDWR, lockFileMode)
			if err != nil {
				if os.IsNotExist(err) {
					// Released between the create and the open: look again.
					continue
				}

				return nil, fmt.Errorf("failed to open takeover marker %s: %w", markerPath, err)
			}
		default:
			return nil, fmt.Errorf("failed to create takeover marker %s: %w", markerPath, err)
		}

		if lockErr := flockExclusive(file); lockErr != nil {
			_ = file.Close()

			if errors.Is(lockErr, ErrLockAlreadyHeld) {
				logger.L().Debug(ctx, "Takeover marker is held by a live writer",
					zap.String("path", markerPath))
			}

			return nil, lockErr
		}

		// Re-verify under the lock: the path may have been released and
		// re-created (or replaced by an older build's writer) in between.
		pathInfo, statErr := os.Stat(markerPath)
		fileInfo, fstatErr := file.Stat()

		switch {
		case fstatErr != nil:
			_ = file.Close()

			return nil, fmt.Errorf("failed to stat takeover marker handle %s: %w", markerPath, fstatErr)
		case statErr != nil:
			_ = file.Close()

			if os.IsNotExist(statErr) {
				continue
			}

			return nil, fmt.Errorf("failed to stat takeover marker %s: %w", markerPath, statErr)
		case !os.SameFile(pathInfo, fileInfo):
			_ = file.Close()

			continue
		}

		if !created {
			logger.L().Warn(ctx, "Recovering a takeover marker left by a dead writer",
				zap.String("path", markerPath))
		}

		return file, nil
	}

	return nil, ErrLockAlreadyHeld
}

// flockExclusive takes the exclusive, non-blocking flock that is the marker's
// ownership. Contention is reported as ErrLockAlreadyHeld; the call never
// blocks.
func flockExclusive(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}

		if errors.Is(err, syscall.EINTR) {
			continue
		}

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLockAlreadyHeld
		}

		return fmt.Errorf("failed to lock takeover marker %s: %w", file.Name(), err)
	}
}

// releaseTakeoverMarker removes the marker once the change to the lock path is
// done. Only the holder — the process holding the marker's flock — removes it,
// and only when the path still names the file that was locked: a marker
// replaced by an older build's writer, which does not take the flock, is left
// to its new holder. The remove happens before the close, while the lock is
// still held.
func releaseTakeoverMarker(ctx context.Context, file *os.File) {
	if file == nil {
		return
	}

	markerPath := file.Name()

	pathInfo, statErr := os.Stat(markerPath)
	fileInfo, fstatErr := file.Stat()

	switch {
	case statErr != nil:
		if !os.IsNotExist(statErr) {
			logger.L().Warn(ctx, "Failed to stat takeover marker",
				zap.String("path", markerPath),
				zap.Error(statErr))
		}
	case fstatErr != nil:
		logger.L().Warn(ctx, "Failed to stat takeover marker handle",
			zap.String("path", markerPath),
			zap.Error(fstatErr))
	case os.SameFile(pathInfo, fileInfo):
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			logger.L().Warn(ctx, "Failed to remove takeover marker",
				zap.String("path", markerPath),
				zap.Error(err))
		}
	default:
		logger.L().Warn(ctx, "Takeover marker was replaced while held; leaving it to its new holder",
			zap.String("path", markerPath))
	}

	if err := file.Close(); err != nil {
		logger.L().Warn(ctx, "Failed to close takeover marker",
			zap.String("path", markerPath),
			zap.Error(err))
	}
}

// createExclusive creates a file that must not exist yet.
func createExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, lockFileMode)
}

// createLockFileOrContention creates the lock file, reporting a lock that
// appeared anyway as contention: only writers that do not take the takeover
// marker (an older version of this package) can create one concurrently.
func createLockFileOrContention(lockPath string) (*os.File, error) {
	file, err := createLockFile(lockPath)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrLockAlreadyHeld
		}

		return nil, err
	}

	return file, nil
}

// createLockFile creates the lock file exclusively and writes the owner token
// that ReleaseLock compares before removing it.
func createLockFile(lockPath string) (*os.File, error) {
	file, err := createExclusive(lockPath)
	if err != nil {
		return nil, err
	}

	if _, err := file.WriteString(uuid.NewString()); err != nil {
		_ = file.Close()
		_ = os.Remove(lockPath)

		return nil, fmt.Errorf("failed to write lock owner token: %w", err)
	}

	return file, nil
}

// ReleaseLock releases a previously acquired lock. The lock file is removed
// only while it still carries this holder's owner token: after a stale
// takeover the path belongs to the new holder, and the old holder's release
// must not delete it. The delete runs under the takeover marker, so it cannot
// interleave with a create or a stale takeover; when the marker is held, a
// path change is already in flight and the release defers to it.
func ReleaseLock(ctx context.Context, file *os.File) error {
	if file == nil {
		return nil
	}

	lockPath := file.Name()

	ownerToken, tokenErr := readOwnerToken(file)

	if closeErr := file.Close(); closeErr != nil {
		logger.L().Warn(ctx, "Failed to close lock file",
			zap.String("path", lockPath),
			zap.Error(closeErr))
	}

	if tokenErr != nil {
		return fmt.Errorf("failed to read lock owner token: %w", tokenErr)
	}

	marker, err := lockTakeoverMarker(ctx, lockPath)
	if err != nil {
		if errors.Is(err, ErrLockAlreadyHeld) {
			// A create or takeover is in flight under the marker: it observed
			// this lock and will keep or replace it deliberately.
			logger.L().Debug(ctx, "Release deferred to an in-flight change of the lock path",
				zap.String("path", lockPath))

			return nil
		}

		return fmt.Errorf("failed to serialize lock release: %w", err)
	}

	defer releaseTakeoverMarker(ctx, marker)

	currentToken, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Already released, taken over, or never published.
			return nil
		}

		return fmt.Errorf("failed to read lock file %s: %w", lockPath, err)
	}

	if string(currentToken) != ownerToken {
		logger.L().Warn(ctx, "Lock file was taken over while held; leaving it to its new holder",
			zap.String("path", lockPath))

		return nil
	}

	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		logger.L().Warn(ctx, "Failed to remove lock file",
			zap.String("path", lockPath),
			zap.Error(err))

		return fmt.Errorf("failed to remove lock file: %w", err)
	}

	return nil
}

// readOwnerToken reads back the token this holder wrote into the lock file.
func readOwnerToken(file *os.File) (string, error) {
	buf := make([]byte, 64)

	n, err := file.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}

	return string(buf[:n]), nil
}
