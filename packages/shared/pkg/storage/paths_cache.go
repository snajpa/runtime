package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// cacheDirName is the fixed path component between the build id and the
// per-instance identifier in the cache directory layout.
const cacheDirName = "cache"

// CachePaths point at one cache instance's directory:
//
//	<TemplateCacheDir>/<BuildID>/cache/<CacheIdentifier>
//
// The identifier is a fresh UUID per instance. Cache state (template chunks,
// frames, COW diffs, size sidecars) is orchestrator-internal and never shared
// between instances, so closing an instance removes only its own directory.
type CachePaths struct {
	Paths

	// CacheIdentifier distinguishes this instance from every other cache of
	// the same build, so closing it can never delete a sibling's files.
	CacheIdentifier string

	config Config
}

func (p Paths) Cache(config Config) (CachePaths, error) {
	identifier, err := uuid.NewRandom()
	if err != nil {
		return CachePaths{}, fmt.Errorf("failed to generate identifier: %w", err)
	}

	paths := CachePaths{
		Paths:           p,
		CacheIdentifier: identifier.String(),
		config:          config,
	}

	if err := paths.createCacheDir(); err != nil {
		return CachePaths{}, err
	}

	return paths, nil
}

func (c CachePaths) CacheSnapfile() string {
	return filepath.Join(c.cacheDir(), SnapfileName)
}

func (c CachePaths) CacheMetadata() string {
	return filepath.Join(c.cacheDir(), MetadataName)
}

func (c CachePaths) cacheDir() string {
	return filepath.Join(c.config.TemplateCacheDir, c.BuildID, cacheDirName, c.CacheIdentifier)
}

// createCacheDir creates the per-instance cache directory and enforces the
// cache permissions policy (CacheDirPerm) on the directories this cache owns:
// the <TemplateCacheDir>/<BuildID>/cache parent and the instance leaf.
// MkdirAll applies its create mode only to directories it creates and always
// filters it through the umask, and it leaves existing directories as they
// are, so the modes are set explicitly; this also tightens directories that
// older releases left world-writable (os.ModePerm).
func (c CachePaths) createCacheDir() error {
	dir := c.cacheDir()

	if err := os.MkdirAll(dir, CacheDirPerm); err != nil {
		return fmt.Errorf("failed to create cache dir '%s': %w", dir, err)
	}

	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, CacheDirPerm); err != nil {
			return fmt.Errorf("failed to set permissions on cache dir '%s': %w", d, err)
		}
	}

	return nil
}

// Close removes this instance's cache directory.
//
// It refuses to remove anything that is not this instance's own
// <...>/cache/<uuid> directory: the identifier must be the canonical form of
// a UUID and the path must carry the cache layout, so a corrupted or tampered
// CachePaths value cannot redirect the removal at another directory. A path
// that cannot be inspected (missing, or replaced by a non-directory) is
// reported as an error rather than silently skipped.
func (c CachePaths) Close() error {
	dir, err := c.checkedCacheDir()
	if err != nil {
		return err
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("failed to stat cache dir '%s': %w", dir, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("refusing to remove cache path '%s': not a directory", dir)
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to remove cache dir '%s': %w", dir, err)
	}

	return nil
}

// checkedCacheDir re-derives this instance's cache directory from its fields
// and verifies that it has the expected <TemplateCacheDir>/<BuildID>/cache/
// <uuid> layout.
func (c CachePaths) checkedCacheDir() (string, error) {
	if c.config.TemplateCacheDir == "" || c.BuildID == "" {
		return "", errors.New("refusing to remove cache path: incomplete cache path configuration")
	}

	// The identifier is joined into the path, so it must be exactly the
	// canonical UUID this instance generated — never an arbitrary string that
	// could escape the cache directory through path elements.
	identifier, err := uuid.Parse(c.CacheIdentifier)
	if err != nil || identifier.String() != c.CacheIdentifier {
		return "", fmt.Errorf("refusing to remove cache path: invalid cache identifier %q", c.CacheIdentifier)
	}

	dir := filepath.Join(c.config.TemplateCacheDir, c.BuildID, cacheDirName, c.CacheIdentifier)
	if filepath.Base(dir) != c.CacheIdentifier || filepath.Base(filepath.Dir(dir)) != cacheDirName {
		return "", fmt.Errorf("refusing to remove cache path '%s': unexpected cache layout", dir)
	}

	return dir, nil
}
