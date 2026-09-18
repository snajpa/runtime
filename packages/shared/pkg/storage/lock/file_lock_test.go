package lock

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTryAcquireLock_Success(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "test-resource-1")

	// Create the directory for the lock
	err := os.MkdirAll(testPath, 0o755)
	require.NoError(t, err)

	file, err := TryAcquireLock(ctx, testPath)

	require.NoError(t, err)
	assert.NotNil(t, file)

	// Verify lock file exists
	lockPath := getLockFilePath(testPath)
	_, err = os.Stat(lockPath)
	require.NoError(t, err)

	// Clean up
	err = ReleaseLock(ctx, file)
	require.NoError(t, err)

	// Verify lock file was removed
	_, err = os.Stat(lockPath)
	assert.True(t, os.IsNotExist(err))
}

func TestTryAcquireLock_AlreadyHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "test-resource-2")

	// Create the directory for the lock
	err := os.MkdirAll(testPath, 0o755)
	require.NoError(t, err)

	// First acquisition should succeed
	file1, err1 := TryAcquireLock(ctx, testPath)
	require.NoError(t, err1)
	assert.NotNil(t, file1)
	defer ReleaseLock(ctx, file1)

	// Second acquisition should fail (lock already held)
	file2, err2 := TryAcquireLock(ctx, testPath)
	require.ErrorIs(t, err2, ErrLockAlreadyHeld)
	assert.Nil(t, file2)
}

func TestTryAcquireLock_StaleLock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "test-resource-3")

	// Create lock directory
	err := os.MkdirAll(testPath, 0o755)
	require.NoError(t, err)

	// Create a stale lock file
	lockPath := getLockFilePath(testPath)
	staleLockFile, err := os.Create(lockPath)
	require.NoError(t, err)
	err = staleLockFile.Close()
	require.NoError(t, err)

	// Set modification time to past (older than TTL)
	pastTime := time.Now().Add(-2 * defaultLockTTL)
	err = os.Chtimes(lockPath, pastTime, pastTime)
	require.NoError(t, err)

	// Try to acquire lock - should succeed after cleaning stale lock
	file, err := TryAcquireLock(ctx, testPath)
	require.NoError(t, err)
	assert.NotNil(t, file)

	// Clean up
	err = ReleaseLock(ctx, file)
	require.NoError(t, err)
}

func TestReleaseLock_NilFile(t *testing.T) {
	t.Parallel()
	// Should not panic or error when releasing nil file
	err := ReleaseLock(t.Context(), nil)
	require.NoError(t, err)
}

func TestGetLockFilePath_Consistency(t *testing.T) {
	t.Parallel()
	testPath := "/tmp/test-key"

	// Same path should always produce the same lock file path
	path1 := getLockFilePath(testPath)
	path2 := getLockFilePath(testPath)

	assert.Equal(t, path1, path2)
	assert.Contains(t, path1, testPath)
	assert.Contains(t, path1, ".lock")
}

func TestGetLockFilePath_DifferentPaths(t *testing.T) {
	t.Parallel()
	path1 := "/tmp/resource-1"
	path2 := "/tmp/resource-2"

	// Different paths should produce different lock file paths
	lockPath1 := getLockFilePath(path1)
	lockPath2 := getLockFilePath(path2)

	assert.NotEqual(t, lockPath1, lockPath2)
}

// TestTryAcquireLock_StaleTakeoverKeepsNewHolder pins the owner-token rule:
// after a stale lock is taken over, the old holder's release must not delete
// the new holder's lock.
func TestTryAcquireLock_StaleTakeoverKeepsNewHolder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	testPath := filepath.Join(t.TempDir(), "test-resource-stale")

	first, err := TryAcquireLock(ctx, testPath)
	require.NoError(t, err)

	// Age the lock past the TTL so the next acquisition takes it over.
	lockPath := getLockFilePath(testPath)
	past := time.Now().Add(-2 * defaultLockTTL)
	require.NoError(t, os.Chtimes(lockPath, past, past))

	second, err := TryAcquireLock(ctx, testPath)
	require.NoError(t, err)
	require.NotNil(t, second)

	// The old holder is no longer the owner: its release is a no-op.
	require.NoError(t, ReleaseLock(ctx, first))

	_, err = os.Stat(lockPath)
	require.NoError(t, err, "the new holder's lock survives the old holder's release")

	// The new holder releases normally.
	require.NoError(t, ReleaseLock(ctx, second))

	_, err = os.Stat(lockPath)
	assert.True(t, os.IsNotExist(err), "the new holder's release removes the lock")
}

// TestTryAcquireLock_ConcurrentStaleTakeover pins the atomic takeover: exactly
// one writer may acquire after seeing the same stale lock.
func TestTryAcquireLock_ConcurrentStaleTakeover(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	testPath := filepath.Join(t.TempDir(), "test-resource-race")
	lockPath := getLockFilePath(testPath)

	require.NoError(t, os.WriteFile(lockPath, []byte("stale-owner"), 0o644))
	past := time.Now().Add(-2 * defaultLockTTL)
	require.NoError(t, os.Chtimes(lockPath, past, past))

	const racers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []*os.File
	)

	for range racers {
		wg.Go(func() {
			file, err := TryAcquireLock(ctx, testPath)
			if err != nil {
				assert.ErrorIs(t, err, ErrLockAlreadyHeld)

				return
			}

			mu.Lock()
			winners = append(winners, file)
			mu.Unlock()
		})
	}

	wg.Wait()

	require.Len(t, winners, 1, "exactly one writer may take over a stale lock")
	require.NoError(t, ReleaseLock(ctx, winners[0]))
}
