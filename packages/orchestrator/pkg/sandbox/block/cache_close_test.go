//go:build linux

package block

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TestCacheClosedFailsClosed pins the guard ordering: after Close every
// accessor reports the close instead of silently no-oping, so a closed cache
// can never be mistaken for an empty one and nothing can reach the unmapped
// region through the ordinary API.
func TestCacheClosedFailsClosed(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)
	cache, err := NewCache(blockSize*4, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)

	buf := make([]byte, blockSize)
	_, err = cache.WriteAt(buf, 0)
	require.NoError(t, err)

	require.NoError(t, cache.Close())

	var closedErr *CacheClosedError
	require.ErrorAs(t, cache.Close(), &closedErr, "a second Close must fail closed")

	_, err = cache.ReadAt(buf, 0)
	require.ErrorAs(t, err, &closedErr)

	_, err = cache.WriteAt(buf, 0)
	require.ErrorAs(t, err, &closedErr)

	_, err = cache.WriteAtShared(buf, 0)
	require.ErrorAs(t, err, &closedErr)

	_, err = cache.WriteZeroesAt(0, blockSize)
	require.ErrorAs(t, err, &closedErr)

	_, err = cache.Slice(0, blockSize)
	require.ErrorAs(t, err, &closedErr)

	_, err = cache.sliceDirect(0, blockSize)
	require.ErrorAs(t, err, &closedErr)
}

// TestSliceReturnsOwnedCopy pins the ownership boundary: Slice hands the
// caller its own buffer, so mutating it cannot reach the cache and reading it
// after Close (mapping unmapped, file removed) is safe. Before S-17 the same
// sequence read through a raw mmap alias.
func TestSliceReturnsOwnedCopy(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)
	path := t.TempDir() + "/cache"

	cache, err := NewCache(blockSize*2, blockSize, path, false)
	require.NoError(t, err)

	content := make([]byte, blockSize*2)
	_, err = rand.Read(content)
	require.NoError(t, err)

	_, err = cache.WriteAt(content, 0)
	require.NoError(t, err)

	slice, err := cache.Slice(0, blockSize)
	require.NoError(t, err)
	require.Equal(t, content[:blockSize], slice)

	// Mutating the returned buffer must not reach the cache.
	mutated := make([]byte, len(slice))
	for i := range slice {
		slice[i] ^= 0xFF
		mutated[i] = slice[i]
	}

	again, err := cache.Slice(0, blockSize)
	require.NoError(t, err)
	require.Equal(t, content[:blockSize], again, "Slice must not alias the cache")

	require.NoError(t, cache.Close())
	require.NoFileExists(t, path)

	// Reading the caller-owned buffer after Close must be safe; a raw mmap
	// alias would fault on the unmapped region here.
	require.Equal(t, mutated, slice)
}

// TestCacheCloseRacesWithReadersAndWriters exercises the ownership model under
// the race detector: readers and writers either observe data or a clean close
// error, and nothing faults in the unmapped region.
func TestCacheCloseRacesWithReadersAndWriters(t *testing.T) {
	t.Parallel()

	blockSize := int64(header.PageSize)
	cache, err := NewCache(blockSize*16, blockSize, t.TempDir()+"/cache", false)
	require.NoError(t, err)

	block := make([]byte, blockSize)
	_, err = cache.WriteAt(block, 0)
	require.NoError(t, err)

	const iterations = 200

	var wg sync.WaitGroup

	// Readers: ReadAt and Slice either return data or the close error.
	for range 4 {
		wg.Go(func() {
			buf := make([]byte, blockSize)
			for range iterations {
				if _, err := cache.ReadAt(buf, 0); err != nil {
					if _, ok := errors.AsType[*CacheClosedError](err); !ok {
						t.Errorf("ReadAt after close: unexpected error %v", err)
					}

					return
				}

				if _, err := cache.Slice(0, blockSize); err != nil {
					if _, ok := errors.AsType[*CacheClosedError](err); !ok {
						t.Errorf("Slice after close: unexpected error %v", err)
					}

					return
				}
			}
		})
	}

	// Writers: block-aligned writes into block 0.
	for range 2 {
		wg.Go(func() {
			for range iterations {
				if _, err := cache.WriteAt(block, 0); err != nil {
					if _, ok := errors.AsType[*CacheClosedError](err); !ok {
						t.Errorf("WriteAt after close: unexpected error %v", err)
					}

					return
				}
			}
		})
	}

	// The closer may win at any point; every accessor must tolerate it.
	wg.Go(func() {
		_ = cache.Close()
	})

	wg.Wait()
}
