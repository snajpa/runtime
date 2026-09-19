// Command migrate-builds moves existing build artifacts onto the current write
// format, and reconciles references against the objects that are actually
// present before anything is removed.
//
// Why it exists: header formats are versioned (V3 carries no frame tables; V4
// and V5 do) and the write format is pinned by headerWriteVersion - V5 behind
// the header-v5-write rollout flag, V4 otherwise. A fleet that has been writing
// older formats needs a way to move existing artifacts forward without
// rewriting reads in place. This tool reads each artifact, verifies it against
// the header, rewrites it (header-only when the source already carries frame
// data and no compression change was asked for, payload and header when it does
// not), verifies the result, and reports every action.
//
// Safety properties, deliberately:
//   - dry run by default: -apply (or -dry-run=false) is required to write;
//   - nothing is written for an artifact that does not verify first;
//   - the rewritten artifact is read back and re-verified before it is
//     reported as migrated;
//   - source objects are never deleted implicitly: superseded paths are
//     reported, and removed only with -delete-superseded -confirm;
//   - work is bounded by -concurrency and -rate, and can be resumed: re-running
//     skips artifacts that are already on the target format (idempotent);
//   - a run in which any artifact fails exits non-zero (an artifact that is
//     simply missing is reported, not fatal).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const (
	modeMigrate   = "migrate"
	modeReconcile = "reconcile"

	// defaultTargetHeaderVersion is the write format the runtime pins for new
	// uploads (S-41 / REQ-F2 "one write format going forward").
	defaultTargetHeaderVersion = header.MetadataVersionV5

	defaultConcurrency = 4
	defaultFrameSizeKB = 2048
	defaultLevel       = 2
)

type buildList []string

func (b *buildList) String() string { return strings.Join(*b, ",") }

func (b *buildList) Set(value string) error {
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if _, err := uuid.Parse(part); err != nil {
			return fmt.Errorf("invalid build ID %q: %w", part, err)
		}

		*b = append(*b, part)
	}

	return nil
}

type options struct {
	mode     string
	builds   buildList
	buildsIn string

	storageURL string

	dryRun  bool
	apply   bool
	verify  bool
	confirm bool

	concurrency int
	rate        int
	limit       int

	targetHeaderVersion uint64

	compressType  string
	compressLevel int
	frameSizeKB   int

	deleteSuperseded bool
	deleteOrphans    bool
	scanPrefix       string

	reportPath string
	timeout    time.Duration
}

func main() {
	if err := run(); err != nil {
		log.Printf("migrate-builds: %v", err)
		os.Exit(1)
	}
}

// run holds the work so defers are honored on every path.
func run() error {
	opts := parseFlags()

	if opts.apply {
		opts.dryRun = false
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	switch opts.mode {
	case modeMigrate:
		return runMigrate(ctx, opts)
	case modeReconcile:
		return runReconcile(ctx, opts)
	default:
		return fmt.Errorf("unknown mode %q (want %s or %s)", opts.mode, modeMigrate, modeReconcile)
	}
}

func parseFlags() options {
	var opts options

	flag.StringVar(&opts.mode, "mode", modeMigrate, "migrate | reconcile")
	flag.Var(&opts.builds, "build", "build ID to process (repeatable; comma-separated accepted)")
	flag.StringVar(&opts.buildsIn, "builds-file", "", "file with one build ID per line (# comments allowed)")
	flag.StringVar(&opts.storageURL, "storage-url", "", "storage URL (default: TEMPLATE_STORAGE_URL or the legacy storage env)")

	flag.BoolVar(&opts.dryRun, "dry-run", true, "report what would happen without writing")
	flag.BoolVar(&opts.apply, "apply", false, "write the changes (same as -dry-run=false)")
	flag.BoolVar(&opts.verify, "verify", true, "read artifacts back and verify them (migrate always verifies before writing)")
	flag.BoolVar(&opts.confirm, "confirm", false, "required to perform any deletion")

	flag.IntVar(&opts.concurrency, "concurrency", defaultConcurrency, "artifacts processed in parallel")
	flag.IntVar(&opts.rate, "rate", 0, "maximum artifacts started per second (0 = unbounded)")
	flag.IntVar(&opts.limit, "limit", 0, "stop after this many artifacts (0 = no limit)")

	flag.Uint64Var(&opts.targetHeaderVersion, "target-header-version", defaultTargetHeaderVersion, "header format to migrate to (4 or 5)")

	flag.StringVar(&opts.compressType, "compress-type", "", "re-encode payloads to this codec (zstd | lz4); empty keeps the source payload")
	flag.IntVar(&opts.compressLevel, "compress-level", defaultLevel, "compression level for -compress-type")
	flag.IntVar(&opts.frameSizeKB, "frame-size-kb", defaultFrameSizeKB, "frame size for -compress-type")

	flag.BoolVar(&opts.deleteSuperseded, "delete-superseded", false, "delete payload paths superseded by a migration (requires -confirm)")
	flag.BoolVar(&opts.deleteOrphans, "delete-orphans", false, "delete orphan objects found by -scan-prefix (requires -confirm)")
	flag.StringVar(&opts.scanPrefix, "scan-prefix", "", "reconcile: list objects under this prefix and report missing/orphan objects")

	flag.StringVar(&opts.reportPath, "report", "", "write a JSON report here")
	flag.DurationVar(&opts.timeout, "timeout", 0, "stop after this duration (0 = no timeout)")

	flag.Parse()

	return opts
}

// resolveStorageSpec prefers an explicit -storage-url and falls back to the
// runtime's own storage resolution, so the tool runs against whatever the fleet
// is configured for.
func resolveStorageSpec(opts options) (storage.Spec, error) {
	if opts.storageURL != "" {
		return storage.ParseStorageURL(opts.storageURL)
	}

	return cfg.TemplateStorage()
}

// buildIDs returns the work list, de-duplicated and order-preserved.
func buildIDs(opts options) ([]string, error) {
	all := append([]string{}, opts.builds...)

	if opts.buildsIn != "" {
		data, err := os.ReadFile(opts.buildsIn)
		if err != nil {
			return nil, fmt.Errorf("read builds file: %w", err)
		}

		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			if _, err := uuid.Parse(line); err != nil {
				return nil, fmt.Errorf("builds file line %q: %w", line, err)
			}

			all = append(all, line)
		}
	}

	seen := make(map[string]struct{}, len(all))
	unique := make([]string, 0, len(all))

	for _, id := range all {
		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	if len(unique) == 0 {
		return nil, errors.New("no builds given: use -build or -builds-file")
	}

	return unique, nil
}

// reporter serializes outcomes so a long run leaves a machine-readable record.
type reporter struct {
	mu     sync.Mutex
	out    *os.File
	counts map[string]int
	bytes  int64
}

func newReporter(path string) (*reporter, error) {
	r := &reporter{counts: map[string]int{}}

	if path == "" {
		return r, nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open report: %w", err)
	}

	r.out = f

	return r, nil
}

func (r *reporter) record(outcome *artifactOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counts[outcome.Action]++
	r.bytes += outcome.Bytes

	if r.out == nil {
		return
	}

	encoded, err := json.Marshal(outcome)
	if err != nil {
		log.Printf("migrate-builds: marshal outcome: %v", err)

		return
	}

	if _, err := r.out.Write(append(encoded, '\n')); err != nil {
		log.Printf("migrate-builds: write report: %v", err)
	}
}

func (r *reporter) close() {
	if r.out == nil {
		return
	}

	summary := make(map[string]any, len(r.counts)+1)
	for action, n := range r.counts {
		summary[action] = n
	}

	summary["bytes"] = r.bytes

	if encoded, err := json.Marshal(map[string]any{"summary": summary}); err == nil {
		_, _ = r.out.Write(append(encoded, '\n'))
	}

	_ = r.out.Close()
}

// count returns how many outcomes of the given action were recorded.
func (r *reporter) count(action string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.counts[action]
}

func (r *reporter) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	parts := make([]string, 0, len(r.counts))
	for action, n := range r.counts {
		parts = append(parts, fmt.Sprintf("%s=%d", action, n))
	}

	return strings.Join(parts, " ") + fmt.Sprintf(" bytes=%d", r.bytes)
}

// limiter bounds how many artifacts are started per second.
type limiter struct {
	ticker *time.Ticker
}

func newLimiter(ratePerSecond int) *limiter {
	if ratePerSecond <= 0 {
		return &limiter{}
	}

	return &limiter{ticker: time.NewTicker(time.Second / time.Duration(ratePerSecond))}
}

func (l *limiter) wait(ctx context.Context) error {
	if l.ticker == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ticker.C:
		return nil
	}
}

func (l *limiter) stop() {
	if l.ticker != nil {
		l.ticker.Stop()
	}
}

// forEachArtifact runs fn for every build's artifacts with bounded concurrency
// and rate; it returns the first error but always waits for started work.
func forEachArtifact(ctx context.Context, opts options, builds []string, rep *reporter, fn func(context.Context, string, artifactKind) (*artifactOutcome, error)) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(max(opts.concurrency, 1))

	lim := newLimiter(opts.rate)
	defer lim.stop()

	started := 0

outer:
	for _, build := range builds {
		for _, kind := range artifactKinds() {
			if opts.limit > 0 && started >= opts.limit {
				break outer
			}

			started++

			group.Go(func() error {
				if err := lim.wait(groupCtx); err != nil {
					return err
				}

				outcome, err := fn(groupCtx, build, kind)
				if outcome != nil {
					rep.record(outcome)
					log.Printf("%s %s %s: %s%s", outcome.Build, outcome.Artifact, outcome.Action,
						outcome.FromPath, outcome.Detail)
				}

				return err
			})
		}
	}

	return group.Wait()
}
