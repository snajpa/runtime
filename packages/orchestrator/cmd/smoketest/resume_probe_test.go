//go:build linux

package smoketest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	buildconfig "github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/config"
	buildenvd "github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/envd"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/sandboxtypes"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/templates"
)

const (
	resumeProbeCommandTimeout = 30 * time.Second
	resumeProbeCleanupTimeout = 30 * time.Second
	resumeProbeTimeout        = 10 * time.Minute
)

type immutableProbeMarker struct {
	Path    string
	Content string
	SHA256  string
}

type resumeProbeRequest struct {
	Label   string
	Runtime sandboxtypes.RuntimeMetadata
}

type resumeProbeCommandResult struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

type resumeProbeSandbox interface {
	Close(context.Context) error
	RunCommand(context.Context, string, time.Duration) (resumeProbeCommandResult, error)
	ObservedTransport() rootfs.Transport
}

type resumeProbeResumer func(context.Context, resumeProbeRequest) (resumeProbeSandbox, error)

type resumeProbeOptions struct {
	RunID              string
	ProbePath          string
	RequestedTransport rootfs.Transport
	Tree               string
	CommandTimeout     time.Duration
	CleanupTimeout     time.Duration
}

type resumeProbeSample struct {
	Label                 string
	SandboxID             string
	RestoreToReady        time.Duration
	PostResumeCheck       time.Duration
	PrivatePathReadCheck  time.Duration
	PrivatePathReadAbsent bool
}

type resumeProbeStatus string

const (
	resumeProbePass         resumeProbeStatus = "PASS"
	resumeProbeFail         resumeProbeStatus = "FAIL"
	resumeProbeNotRun       resumeProbeStatus = "NOT_RUN"
	resumeProbeInconclusive resumeProbeStatus = "INCONCLUSIVE"
)

type resumeProbeResult struct {
	RunID                 string
	ProbePath             string
	RequestedTransport    string
	Transport             string
	Tree                  string
	Status                resumeProbeStatus
	Samples               []resumeProbeSample
	IsolationCheck        time.Duration
	IsolationNegativeRead bool
	Isolated              bool
	Accepted              bool
}

// TestSnapshotResumePostReadyIsolation is an explicitly opt-in runtime proof.
// It builds a template whose persisted rootfs contains a known marker, then
// uses the real Factory.ResumeSandbox path (which waits for envd) twice. The
// test is intentionally separate from transport fio and is not run by default
// because it needs Docker, KVM, Firecracker, envd, and the local test stack.
func TestSnapshotResumePostReadyIsolation(t *testing.T) { //nolint:paralleltest // owns the local runtime fixture
	if os.Getenv("E2B_RUN_RESUME_PROBE") != "1" {
		t.Skip("set E2B_RUN_RESUME_PROBE=1 to run the real snapshot/resume proof")
	}

	requestedTransport := configureResumeProbeTransport(t)
	checkPrerequisites(t)
	dataDir := t.TempDir()
	envdPath := findOrBuildEnvd(t)
	setupLocalDirs(t, dataDir)
	setupEnvVars(t, dataDir, envdPath)
	downloadKernel(t, dataDir)
	downloadFC(t, dataDir, featureflags.DefaultFirecrackerVersion)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	envdVersion, err := buildenvd.GetEnvdVersion(ctx, envdPath)
	require.NoError(t, err, "read version from probe envd fixture")

	infra := newTestInfra(t, ctx)
	defer infra.close(ctx)

	buildID := uuid.New().String()
	templateID := "resume-probe-" + uuid.NewString()
	markerPath := "/var/tmp/e2b-resume-immutable-" + uuid.NewString()
	markerContent := "e2b-resume-immutable-marker-" + uuid.NewString()
	markerDigest := sha256.Sum256([]byte(markerContent + "\n"))
	marker := immutableProbeMarker{
		Path:    markerPath,
		Content: markerContent,
		SHA256:  hex.EncodeToString(markerDigest[:]),
	}

	force := true
	_, err = infra.builder.Build(
		ctx,
		storage.Paths{BuildID: buildID},
		buildconfig.TemplateConfig{
			Version:            templates.TemplateV2LatestVersion,
			TemplateID:         templateID,
			Force:              &force,
			VCpuCount:          2,
			MemoryMB:           512,
			DiskSizeMB:         512,
			FreeDiskSizeMB:     512,
			HugePages:          true,
			KernelVersion:      featureflags.DefaultKernelVersion,
			FirecrackerVersion: featureflags.DefaultFirecrackerVersion,
			FromImage:          baseImage,
			StartCmd:           fmt.Sprintf("printf '%%s\\n' '%s' > '%s' && sync", markerContent, markerPath),
		},
		logger.NewNopLogger().Detach(ctx).Core(),
	)
	require.NoError(t, err, "build immutable-marker template")

	tmpl, err := infra.templateCache.GetTemplate(ctx, buildID, false, false)
	require.NoError(t, err)
	meta, err := tmpl.Metadata()
	require.NoError(t, err)

	accessToken := "resume-probe-token"
	sandboxConfig := sandbox.NewConfig(sandbox.Config{
		BaseTemplateID:  templateID,
		Vcpu:            2,
		RamMB:           512,
		TotalDiskSizeMB: 512,
		HugePages:       true,
		SkipEnvdWait:    false, // production ResumeSandbox arm; WaitForEnvd(StartTypeResume) is the readiness boundary.
		Envd: sandbox.EnvdMetadata{
			Vars:        map[string]string{},
			AccessToken: &accessToken,
			Version:     envdVersion,
		},
		FirecrackerConfig: fc.Config{
			KernelVersion:      meta.Template.KernelVersion,
			FirecrackerVersion: meta.Template.FirecrackerVersion,
		},
	})

	requests := []resumeProbeRequest{
		{
			Label: "sandbox-a",
			Runtime: sandboxtypes.RuntimeMetadata{
				TemplateID:  templateID,
				SandboxID:   "resume-probe-a-" + uuid.NewString(),
				ExecutionID: uuid.NewString(),
				TeamID:      "resume-probe",
			},
		},
		{
			Label: "sandbox-b",
			Runtime: sandboxtypes.RuntimeMetadata{
				TemplateID:  templateID,
				SandboxID:   "resume-probe-b-" + uuid.NewString(),
				ExecutionID: uuid.NewString(),
				TeamID:      "resume-probe",
			},
		},
	}

	resumer := func(ctx context.Context, request resumeProbeRequest) (resumeProbeSandbox, error) {
		startedAt := time.Now()
		sbx, err := infra.factory.ResumeSandbox(
			ctx,
			tmpl,
			sandboxConfig,
			request.Runtime,
			startedAt,
			startedAt.Add(resumeProbeTimeout),
			nil,
		)
		if err != nil {
			return nil, err
		}

		return &envdResumeProbeSandbox{sandbox: sbx, transport: sbx.RootfsTransport()}, nil
	}

	result, err := runResumeProbe(ctx, resumer, requests, marker, resumeProbeOptions{
		RunID:              "resume-probe-" + uuid.NewString(),
		RequestedTransport: requestedTransport,
		Tree:               "production-resume",
	})
	if err != nil || !result.Accepted || result.Status != resumeProbePass {
		t.Logf("resume probe result: status=%s accepted=%t requested=%s observed=%s tree=%s samples=%d err=%v", result.Status, result.Accepted, result.RequestedTransport, result.Transport, result.Tree, len(result.Samples), err)
		for i, sample := range result.Samples {
			t.Logf("resume probe sample[%d]: sandbox=%s restore-to-ready=%s post-resume=%s private-read=%t/%s", i, sample.SandboxID, sample.RestoreToReady, sample.PostResumeCheck, sample.PrivatePathReadAbsent, sample.PrivatePathReadCheck)
		}
	}
	if err != nil && result.Status == resumeProbeNotRun {
		t.Skipf("resume probe arm not run: %v", err)
	}
	require.NoError(t, err)
	require.Len(t, result.Samples, 2)
	require.True(t, result.Accepted)
	require.True(t, result.Isolated)
	require.True(t, result.IsolationNegativeRead)
	require.True(t, result.Samples[1].PrivatePathReadAbsent)
	t.Logf("accepted runtime proof: requested-transport=%s observed-transport=%s tree=%s restore-to-ready=%s/%s post-resume=%s/%s B-negative-read=%t/%s isolation-check=%s", result.RequestedTransport, result.Transport, result.Tree, result.Samples[0].RestoreToReady, result.Samples[1].RestoreToReady, result.Samples[0].PostResumeCheck, result.Samples[1].PostResumeCheck, result.Samples[1].PrivatePathReadAbsent, result.Samples[1].PrivatePathReadCheck, result.IsolationCheck)
}

func configureResumeProbeTransport(t *testing.T) rootfs.Transport {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("E2B_RESUME_PROBE_TRANSPORT"))
	if raw == "" {
		raw = string(rootfs.TransportNBD)
	}

	requested := rootfs.Transport(raw)
	switch requested {
	case rootfs.TransportNBD:
		featureflags.OverrideBoolFlag(featureflags.UblkRootfsFlag, false)
	case rootfs.TransportUblk:
		featureflags.OverrideBoolFlag(featureflags.UblkRootfsFlag, true)
	default:
		t.Fatalf("unsupported E2B_RESUME_PROBE_TRANSPORT %q; choose %q or %q", raw, rootfs.TransportNBD, rootfs.TransportUblk)
	}

	return requested
}

// runResumeProbe is the result-boundary adapter. It owns both resumed sandboxes
// until independent-deadline cleanup succeeds; fake resumer tests cover only
// mechanics, while TestSnapshotResumePostReadyIsolation is runtime evidence.
func runResumeProbe(
	ctx context.Context,
	resume resumeProbeResumer,
	requests []resumeProbeRequest,
	marker immutableProbeMarker,
	opts resumeProbeOptions,
) (result resumeProbeResult, err error) {
	if ctx == nil {
		return result, errors.New("resume probe: nil context")
	}
	if resume == nil {
		return result, errors.New("resume probe: nil resumer")
	}
	if len(requests) != 2 {
		return result, fmt.Errorf("resume probe needs exactly two requests, got %d", len(requests))
	}
	if requests[0].Runtime.SandboxID == "" || requests[1].Runtime.SandboxID == "" {
		return result, errors.New("resume probe requires two sandbox IDs")
	}
	if requests[0].Runtime.SandboxID == requests[1].Runtime.SandboxID {
		return result, errors.New("resume probe requires distinct sandbox IDs")
	}
	if marker.Path == "" || marker.Content == "" || marker.SHA256 == "" {
		return result, errors.New("resume probe requires an immutable marker path, content, and digest")
	}
	if opts.RunID == "" {
		opts.RunID = "resume-probe-" + uuid.NewString()
	}
	if opts.ProbePath == "" {
		opts.ProbePath = "/var/tmp/" + opts.RunID
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = resumeProbeCommandTimeout
	}
	if opts.CleanupTimeout <= 0 {
		opts.CleanupTimeout = resumeProbeCleanupTimeout
	}
	if path.Clean(opts.ProbePath) != opts.ProbePath || !strings.HasPrefix(opts.ProbePath, "/var/tmp/") {
		return result, fmt.Errorf("probe path must be a clean path below /var/tmp: %q", opts.ProbePath)
	}
	if path.Clean(marker.Path) != marker.Path || !strings.HasPrefix(marker.Path, "/var/tmp/") {
		return result, fmt.Errorf("immutable marker path must be a clean path below /var/tmp: %q", marker.Path)
	}
	if opts.ProbePath == marker.Path {
		return result, errors.New("probe path must differ from immutable marker path")
	}

	result = resumeProbeResult{
		RunID:              opts.RunID,
		ProbePath:          opts.ProbePath,
		RequestedTransport: string(opts.RequestedTransport),
		Tree:               opts.Tree,
		Status:             resumeProbeFail,
		Samples:            make([]resumeProbeSample, 0, 2),
	}

	var handles []resumeProbeSandbox
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.CleanupTimeout)
		defer cancel()
		for i := len(handles) - 1; i >= 0; i-- {
			if closeErr := handles[i].Close(cleanupCtx); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close sandbox %d: %w", i, closeErr))
				result.Accepted = false
				result.Status = resumeProbeFail
			}
		}
		if err != nil && result.Status != resumeProbeNotRun {
			result.Accepted = false
			result.Status = resumeProbeFail
		}
	}()

	for i, request := range requests {
		if request.Label == "" {
			request.Label = fmt.Sprintf("sandbox-%d", i)
		}
		started := time.Now()
		handle, resumeErr := resume(ctx, request)
		readyDuration := time.Since(started)
		if handle != nil {
			handles = append(handles, handle)
		}
		if resumeErr != nil {
			return result, fmt.Errorf("resume %q: %w", request.Label, resumeErr)
		}
		if handle == nil {
			return result, fmt.Errorf("resume %q returned nil sandbox", request.Label)
		}

		observedTransport := handle.ObservedTransport()
		if observedTransport == "" {
			result.Status = resumeProbeNotRun
			return result, fmt.Errorf("resume %q returned no observed rootfs transport", request.Label)
		}
		if i == 0 {
			result.Transport = string(observedTransport)
		} else if result.Transport != string(observedTransport) {
			result.Status = resumeProbeNotRun
			return result, fmt.Errorf("resume transport changed between sandboxes: %q then %q", result.Transport, observedTransport)
		}
		if opts.RequestedTransport != "" && observedTransport != opts.RequestedTransport {
			result.Status = resumeProbeNotRun
			return result, fmt.Errorf("requested rootfs transport %q but ResumeSandbox observed %q", opts.RequestedTransport, observedTransport)
		}

		privatePathReadDuration := time.Duration(0)
		privatePathReadAbsent := false
		if i == 1 {
			privatePathReadStarted := time.Now()
			negativeRead, negativeReadErr := handle.RunCommand(ctx, negativePrivatePathReadScript(opts.ProbePath, markerFor(opts.RunID, 0)), opts.CommandTimeout)
			privatePathReadDuration = time.Since(privatePathReadStarted)
			if negativeReadErr != nil {
				return result, fmt.Errorf("sandbox B negative-read check: %w", negativeReadErr)
			}
			if negativeRead.ExitCode != 0 {
				return result, fmt.Errorf("sandbox B read sandbox A private path before write: %s", strings.TrimSpace(negativeRead.Stderr))
			}
			privatePathReadAbsent = true
			result.IsolationNegativeRead = true
		}

		probeStarted := time.Now()
		commandResult, commandErr := handle.RunCommand(ctx, resumeProbeScript(marker, opts.ProbePath, markerFor(opts.RunID, i)), opts.CommandTimeout)
		postResumeDuration := time.Since(probeStarted)
		result.Samples = append(result.Samples, resumeProbeSample{
			Label:                 request.Label,
			SandboxID:             request.Runtime.SandboxID,
			RestoreToReady:        readyDuration,
			PostResumeCheck:       postResumeDuration,
			PrivatePathReadCheck:  privatePathReadDuration,
			PrivatePathReadAbsent: privatePathReadAbsent,
		})
		if commandErr != nil {
			return result, fmt.Errorf("post-resume check %q: %w", request.Label, commandErr)
		}
		if commandResult.ExitCode != 0 {
			return result, fmt.Errorf("post-resume check %q exited %d: %s", request.Label, commandResult.ExitCode, strings.TrimSpace(commandResult.Stderr))
		}
	}

	isolationStarted := time.Now()
	verifyA, verifyErr := handles[0].RunCommand(ctx, verifyProbeScript(opts.ProbePath, markerFor(opts.RunID, 0)), opts.CommandTimeout)
	result.IsolationCheck = time.Since(isolationStarted)
	if verifyErr != nil {
		return result, fmt.Errorf("re-read sandbox A after sandbox B write: %w", verifyErr)
	}
	if verifyA.ExitCode != 0 {
		return result, fmt.Errorf("sandbox A changed after sandbox B write: %s", strings.TrimSpace(verifyA.Stderr))
	}

	result.Isolated = true
	result.Accepted = true
	result.Status = resumeProbePass
	return result, nil
}

func markerFor(runID string, index int) string {
	return fmt.Sprintf("%s-sandbox-%d", runID, index)
}

func resumeProbeScript(marker immutableProbeMarker, probePath, writableMarker string) string {
	return fmt.Sprintf(`set -eu
immutable=%s
expected_content=%s
expected_digest=%s
probe=%s
marker=%s
modified=%s
test "$(cat "$immutable")" = "$expected_content"
test "$(sha256sum "$immutable" | cut -d ' ' -f 1)" = "$expected_digest"
test ! -e "$probe"
printf '%%s\n' "$marker" > "$probe"
sync
test "$(cat "$probe")" = "$marker"
printf '%%s\n' "$modified" >> "$probe"
sync
test "$(cat "$probe")" = "$(printf '%%s\n%%s' "$marker" "$modified")"
`, shellQuote(marker.Path), shellQuote(marker.Content), shellQuote(marker.SHA256), shellQuote(probePath), shellQuote(writableMarker), shellQuote(writableMarker+"-modified"))
}

func negativePrivatePathReadScript(probePath, firstWritableMarker string) string {
	return fmt.Sprintf(`set -eu
probe=%s
expected=%s
if actual=$(cat "$probe" 2>/dev/null); then
printf 'sandbox B read private token %%s; expected path to be absent (A token %%s)\n' "$actual" "$expected" >&2
exit 41
fi
test ! -e "$probe"
`, shellQuote(probePath), shellQuote(firstWritableMarker))
}

func verifyProbeScript(probePath, writableMarker string) string {
	return fmt.Sprintf(`set -eu
probe=%s
marker=%s
modified=%s
test "$(cat "$probe")" = "$(printf '%%s\n%%s' "$marker" "$modified")"
`, shellQuote(probePath), shellQuote(writableMarker), shellQuote(writableMarker+"-modified"))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

type envdResumeProbeSandbox struct {
	sandbox   *sandbox.Sandbox
	transport rootfs.Transport
}

func (s *envdResumeProbeSandbox) ObservedTransport() rootfs.Transport {
	return s.transport
}

func (s *envdResumeProbeSandbox) Close(ctx context.Context) error {
	return s.sandbox.Close(ctx)
}

func (s *envdResumeProbeSandbox) RunCommand(ctx context.Context, script string, timeout time.Duration) (resumeProbeCommandResult, error) {
	stream, err := s.sandbox.StartEnvdShell(ctx, "/bin/sh", []string{"-c", script}, "root", timeout)
	if err != nil {
		return resumeProbeCommandResult{}, fmt.Errorf("start envd command: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var end *process.ProcessEvent_EndEvent
	for stream.Receive() {
		event := stream.Msg().GetEvent().GetEvent()
		switch event := event.(type) {
		case *process.ProcessEvent_Data:
			if data := event.Data.GetStdout(); data != nil {
				_, _ = stdout.Write(data)
			}
			if data := event.Data.GetStderr(); data != nil {
				_, _ = stderr.Write(data)
			}
		case *process.ProcessEvent_End:
			end = event.End
		}
	}
	if streamErr := stream.Err(); streamErr != nil {
		return resumeProbeCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("envd command stream: %w", streamErr)
	}
	if end == nil {
		return resumeProbeCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, errors.New("envd command stream ended without an exit event")
	}

	return resumeProbeCommandResult{
		ExitCode: end.GetExitCode(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}
