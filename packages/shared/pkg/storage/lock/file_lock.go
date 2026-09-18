package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	// defaultLockTTL is the time after which a lock file is considered stale
	// (its owner either finished without releasing or died). Stale locks are
	// taken over atomically, never deleted in place.
	defaultLockTTL = 10 * time.Second
	// lockFileMode is the file mode for lock files. The lock is advisory and
	// holds only the owner token.
	lockFileMode = 0o644
	// staleTakeoverAttempts bounds the stat/takeover/create retries when
	// several writers race to take over the same stale lock.
	staleTakeoverAttempts = 3
)

var ErrLockAlreadyHeld = errors.New("lock is already held by another process")

// getLockFilePath generates a lock file path from a key
func getLockFilePath(path string) string {
	return path + ".lock"
}

// TryAcquireLock attempts to acquire the advisory cache lock for path.
//
// Contention is reported as ErrLockAlreadyHeld and never blocks: callers
// either retry with backoff (the cache-fill writebacks do, see
// retryContendedLock) or treat it as normal cache dedup and skip the write.
//
// A lock whose mtime is older than defaultLockTTL is stale. Takeover is
// atomic: the stale file is renamed to a unique tombstone and only the process
// that wins that rename creates the new lock, so two writers can never both
// acquire after seeing the same stale lock. The owner token written into the
// lock file keeps ReleaseLock from deleting a lock that was taken over.
func TryAcquireLock(ctx context.Context, path string) (*os.File, error) {
	lockPath := getLockFilePath(path)

	for range staleTakeoverAttempts {
		info, err := os.Stat(lockPath)
		switch {
		case err == nil && time.Since(info.ModTime()) <= defaultLockTTL:
			// Fresh lock: held by another writer.
			return nil, ErrLockAlreadyHeld
		case err == nil:
			logger.L().Debug(ctx, "Found stale lock file, attempting atomic takeover",
				zap.String("path", lockPath),
				zap.Duration("age", time.Since(info.ModTime())))

			if err := takeOverStaleLock(ctx, lockPath); err != nil {
				return nil, err
			}

			continue
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("failed to stat lock file %s: %w", lockPath, err)
		}

		file, err := createLockFile(lockPath)
		if err == nil {
			return file, nil
		}
		if os.IsExist(err) {
			return nil, ErrLockAlreadyHeld
		}

		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	return nil, ErrLockAlreadyHeld
}

// createLockFile creates the lock file exclusively and writes the owner token
// that ReleaseLock compares before removing it.
func createLockFile(lockPath string) (*os.File, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, lockFileMode)
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

// takeOverStaleLock removes a stale lock by renaming it to a unique tombstone
// and deleting that. Rename is atomic, so of all the processes that see the
// same stale lock exactly one wins; the losers observe ENOENT and simply
// retry the create.
func takeOverStaleLock(ctx context.Context, lockPath string) error {
	tombstone := fmt.Sprintf("%s.stale.%s", lockPath, uuid.NewString())

	if err := os.Rename(lockPath, tombstone); err != nil {
		if os.IsNotExist(err) {
			// Lost the takeover race; the winner already removed the lock.
			return nil
		}

		return fmt.Errorf("failed to take over stale lock %s: %w", lockPath, err)
	}

	if err := os.Remove(tombstone); err != nil && !os.IsNotExist(err) {
		logger.L().Warn(ctx, "Failed to remove stale lock tombstone",
			zap.String("path", tombstone),
			zap.Error(err))
	}

	return nil
}

// ReleaseLock releases a previously acquired lock. The lock file is removed
// only while it still carries this holder's owner token: after a stale
// takeover the path belongs to the new holder, and the old holder's release
// must not delete it.
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

	currentToken, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Already released, taken over, or never published.
			return nil
		}

		return fmt.Errorf("failed to read lock file %s: %w", lockPath, err)
	}

	if string(currentToken) != ownerToken {
		logger.L().Debug(ctx, "Lock was taken over; leaving it to its new holder",
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
