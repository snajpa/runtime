package storage

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Object paths and prefixes cross a trust boundary: peers, the control plane,
// CLI tools and signed upload requests all supply them, and the storage layer
// resolves them against a base directory. These helpers are the single place
// that decides whether a caller-supplied name may be resolved at all — the FS
// provider's prefix delete is irreversible, and cache writeback lands on the
// node's disk.

// ErrInvalidStoragePath marks a caller-supplied object path or prefix the
// storage layer refuses to resolve. Typed so an HTTP edge can map it to a
// client error instead of reporting an internal failure.
var ErrInvalidStoragePath = errors.New("invalid storage path")

// ValidateRelativePath reports whether p may be resolved against a storage
// base directory. It rejects:
//
//   - "" — an empty name is never an object and never a safe delete prefix;
//   - absolute paths, both POSIX ("/…") and the Windows drive-letter forms;
//   - a NUL byte or a backslash (a separator where join semantics differ);
//   - any ".." path element.
//
// Safe but non-clean forms ("a//b", "a/./b", a trailing slash) are accepted:
// filepath.Join normalizes them, which is what readers of existing artifacts
// expect.
func ValidateRelativePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidStoragePath)
	}

	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: contains a NUL byte", ErrInvalidStoragePath)
	}

	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("%w: contains a backslash: %q", ErrInvalidStoragePath, p)
	}

	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: absolute path: %q", ErrInvalidStoragePath, p)
	}

	// "C:", "C:/x": not absolute to path.IsAbs on every platform, never a
	// POSIX-relative name either.
	if len(p) >= 2 && p[1] == ':' {
		return fmt.Errorf("%w: drive-letter form: %q", ErrInvalidStoragePath, p)
	}

	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("%w: %q escapes the base directory", ErrInvalidStoragePath, p)
	}

	if path.Clean(p) == "." {
		return fmt.Errorf("%w: %q names no object", ErrInvalidStoragePath, p)
	}

	return nil
}

// ContainedPath joins rel onto base and proves the result cannot leave base:
// rel must pass ValidateRelativePath, and filepath.Rel re-checks the joined
// result so a join surprise fails closed too.
func ContainedPath(base, rel string) (string, error) {
	if err := ValidateRelativePath(rel); err != nil {
		return "", err
	}

	joined := filepath.Join(base, filepath.FromSlash(rel))

	within, err := filepath.Rel(base, joined)
	if err != nil {
		return "", fmt.Errorf("%w: %q does not resolve under the base directory: %w", ErrInvalidStoragePath, rel, err)
	}

	if within == "." || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the base directory", ErrInvalidStoragePath, rel)
	}

	return joined, nil
}
