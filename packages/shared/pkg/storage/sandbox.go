package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type SandboxFiles struct {
	CachePaths

	SandboxID string
	tmpDir    string
	// We use random id to avoid collision between the paused and restored sandbox caches
	randomID string
}

type Config struct {
	CompressConfig

	SandboxCacheDir  string `env:"SANDBOX_CACHE_DIR,expand"  envDefault:"${ORCHESTRATOR_BASE_PATH}/sandbox"`
	TemplateCacheDir string `env:"TEMPLATE_CACHE_DIR,expand" envDefault:"${ORCHESTRATOR_BASE_PATH}/template"`
}

func (c CachePaths) NewSandboxFiles(sandboxID string) *SandboxFiles {
	randomID := id.Generate()

	return &SandboxFiles{
		CachePaths: c,
		SandboxID:  sandboxID,
		randomID:   randomID,
		tmpDir:     os.TempDir(),
	}
}

func (c CachePaths) NewSandboxFilesWithStaticID(sandboxID string, staticID string) *SandboxFiles {
	return &SandboxFiles{
		CachePaths: c,
		SandboxID:  sandboxID,
		randomID:   staticID,
		tmpDir:     os.TempDir(),
	}
}

func (s *SandboxFiles) SandboxCacheRootfsPath(config Config) string {
	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("rootfs-%s-%s.cow", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxFirecrackerSocketPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("fc-%s-%s.sock", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxUffdSocketPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("uffd-%s-%s.sock", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxCacheRootfsLinkPath(config Config) string {
	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("rootfs-%s-%s.link", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxMetricsFifoPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("fc-metrics-%s-%s.fifo", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxCgroupName() string {
	return fmt.Sprintf("sbx-%s-%s", s.SandboxID, s.randomID)
}

// SandboxFileGlobs returns glob patterns matching the on-disk files a sandbox
// creates: firecracker/uffd sockets and the metrics fifo under tempDir, plus
// the rootfs overlay and link files under sandboxCacheDir. The patterns mirror
// the path builders above and are the single source of truth used by startup
// reclaim. sandboxCacheDir may be empty, in which case the cache patterns are
// omitted.
func SandboxFileGlobs(tempDir, sandboxCacheDir string) []string {
	patterns := []string{
		filepath.Join(tempDir, "fc-*-*.sock"),
		filepath.Join(tempDir, "uffd-*-*.sock"),
		filepath.Join(tempDir, "fc-metrics-*-*.fifo"),
	}
	if sandboxCacheDir != "" {
		patterns = append(patterns,
			filepath.Join(sandboxCacheDir, "rootfs-*-*.cow"),
			filepath.Join(sandboxCacheDir, "rootfs-*-*.link"),
		)
	}

	return patterns
}

// ReclaimSandboxFiles removes leaked sandbox files matching SandboxFileGlobs,
// left over from sandboxes that did not shut down cleanly. It returns the number
// of files removed and any per-file removal failures. Files that no longer exist
// are treated as already reclaimed.
func ReclaimSandboxFiles(tempDir, sandboxCacheDir string) (int, []error) {
	paths, err := matchingSandboxFiles(tempDir, sandboxCacheDir)
	if err != nil {
		return 0, []error{err}
	}

	reclaimed := 0
	var failures []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Errorf("failed to remove %s: %w", path, err))

			continue
		}

		reclaimed++
	}

	return reclaimed, failures
}

func matchingSandboxFiles(tempDir, sandboxCacheDir string) ([]string, error) {
	paths := make([]string, 0)
	for _, pattern := range SandboxFileGlobs(tempDir, sandboxCacheDir) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to glob %s: %w", pattern, err)
		}

		paths = append(paths, matches...)
	}

	slices.Sort(paths)

	return paths, nil
}

// EnvdSwapStagePrefix is the directory-name prefix the offline envd swap uses
// for its per-swap staging directories under the orchestrator base dir. The
// swap creates it; startup reclaim globs it.
const EnvdSwapStagePrefix = ".envd-swap-"

// EnvdSwapStageOwnerFile is the file a swap stage writes to record the process
// that created it. Reclaim fences on it: a stage whose owner is still alive
// belongs to an active swap — possibly another instance's on a shared host —
// and is never removed.
const EnvdSwapStageOwnerFile = "owner.pid"

// RecordStageOwner writes the calling process's pid into a staging directory.
// The file is owner-only: the jailed debugfs never needs it, only the
// orchestrator's own reclaim does.
func RecordStageOwner(dir string) error {
	owner := filepath.Join(dir, EnvdSwapStageOwnerFile)
	if err := os.WriteFile(owner, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("record swap stage owner in %s: %w", dir, err)
	}

	return nil
}

// StageOwnerAlive reports whether a staging directory's recorded owner is
// still running. ok is false when there is no owner record at all — a legacy
// stage, or not a stage directory — and callers must not read that as
// permission to delete: reclaim only removes what it can prove abandoned.
func StageOwnerAlive(dir, procDir string) (alive, ok bool) {
	raw, err := os.ReadFile(filepath.Join(dir, EnvdSwapStageOwnerFile))
	if err != nil {
		return false, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false, false
	}

	if _, err := os.Stat(filepath.Join(procDir, strconv.Itoa(pid))); err != nil {
		return false, true
	}

	return true, true
}

// StagedDirGlobs returns the patterns matching per-operation staging
// directories that leak when the orchestrator dies mid-operation. An empty
// stageRoot yields no patterns (the caller reports that omission).
func StagedDirGlobs(stageRoot string) []string {
	if stageRoot == "" {
		return nil
	}

	return []string{filepath.Join(stageRoot, EnvdSwapStagePrefix+"*")}
}

// ReclaimStagedDirs removes abandoned staging directories matching
// StagedDirGlobs, recursively. A directory whose recorded owner is still alive
// is skipped and logged — on a shared host it can belong to another
// instance's active swap — and so is a directory without an owner record,
// which reclaim cannot prove abandoned; the log line is the operator's cue to
// clear a leaked legacy directory by hand. Entries that no longer exist are
// treated as already reclaimed. It returns the number of directories removed
// and any per-directory failures.
func ReclaimStagedDirs(ctx context.Context, stageRoot, procDir string) (int, []error) {
	if procDir == "" {
		procDir = "/proc"
	}

	reclaimed := 0
	var failures []error

	for _, pattern := range StagedDirGlobs(stageRoot) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			failures = append(failures, fmt.Errorf("failed to glob %s: %w", pattern, err))

			continue
		}

		for _, path := range matches {
			alive, ok := StageOwnerAlive(path, procDir)
			if !ok {
				logger.L().Warn(ctx, "not reclaiming a swap stage dir without an owner record; clear it manually if it is stale",
					zap.String("path", path))

				continue
			}
			if alive {
				logger.L().Warn(ctx, "not reclaiming a swap stage dir owned by a live process",
					zap.String("path", path))

				continue
			}

			if err := os.RemoveAll(path); err != nil {
				failures = append(failures, fmt.Errorf("failed to remove %s: %w", path, err))

				continue
			}

			reclaimed++
		}
	}

	return reclaimed, failures
}
