//go:build linux

package smoketest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/shared/pkg/sandboxtypes"
)

type fakeResumeProbeSandbox struct {
	id        string
	commands  []string
	closeErr  error
	transport rootfs.Transport
	closed    bool
	order     *[]string
}

func (s *fakeResumeProbeSandbox) ObservedTransport() rootfs.Transport {
	return s.transport
}

func (s *fakeResumeProbeSandbox) Close(context.Context) error {
	s.closed = true
	if s.order != nil {
		*s.order = append(*s.order, s.id)
	}

	return s.closeErr
}

func (s *fakeResumeProbeSandbox) RunCommand(_ context.Context, script string, _ time.Duration) (resumeProbeCommandResult, error) {
	s.commands = append(s.commands, script)

	return resumeProbeCommandResult{ExitCode: 0}, nil
}

func TestRunResumeProbeChecksLiveIsolationAndCleanupOrder(t *testing.T) {
	closeOrder := make([]string, 0, 2)
	handles := map[string]*fakeResumeProbeSandbox{
		"a": {id: "a", transport: rootfs.TransportNBD, order: &closeOrder},
		"b": {id: "b", transport: rootfs.TransportNBD, order: &closeOrder},
	}
	requests := []resumeProbeRequest{
		{Label: "a", Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "sandbox-a"}},
		{Label: "b", Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "sandbox-b"}},
	}
	marker := immutableProbeMarker{
		Path:    "/var/tmp/immutable-marker",
		Content: "known-marker",
		SHA256:  "known-digest",
	}

	result, err := runResumeProbe(
		context.Background(),
		func(_ context.Context, request resumeProbeRequest) (resumeProbeSandbox, error) {
			return handles[request.Label], nil
		},
		requests,
		marker,
		resumeProbeOptions{RunID: "unit", ProbePath: "/var/tmp/unit-probe"},
	)

	require.NoError(t, err)
	require.True(t, result.Accepted)
	require.Equal(t, resumeProbePass, result.Status)
	require.True(t, result.Isolated)
	require.True(t, result.IsolationNegativeRead)
	require.True(t, result.Samples[1].PrivatePathReadAbsent)
	require.Len(t, result.Samples, 2)
	require.Len(t, handles["a"].commands, 2, "A is re-read after B writes")
	require.Len(t, handles["b"].commands, 2, "B negative-reads A before writing")
	require.Contains(t, handles["a"].commands[0], "test ! -e")
	require.Contains(t, handles["a"].commands[0], "known-digest")
	require.Contains(t, handles["b"].commands[0], "actual=$(cat")
	require.Contains(t, handles["b"].commands[0], "unit-sandbox-0")
	require.Contains(t, handles["b"].commands[1], "unit-sandbox-1")
	require.Contains(t, handles["a"].commands[1], "unit-sandbox-0-modified")
	require.Equal(t, []string{"b", "a"}, closeOrder, "both handles stay live until both probes finish")
}

func TestRunResumeProbeRejectsUnexpectedObservedTransport(t *testing.T) {
	handles := map[string]*fakeResumeProbeSandbox{
		"a": {id: "a", transport: rootfs.TransportNBD},
		"b": {id: "b", transport: rootfs.TransportNBD},
	}
	requests := []resumeProbeRequest{
		{Label: "a", Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "sandbox-a"}},
		{Label: "b", Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "sandbox-b"}},
	}
	marker := immutableProbeMarker{Path: "/var/tmp/immutable-marker", Content: "known-marker", SHA256: "known-digest"}

	result, err := runResumeProbe(
		context.Background(),
		func(_ context.Context, request resumeProbeRequest) (resumeProbeSandbox, error) {
			return handles[request.Label], nil
		},
		requests,
		marker,
		resumeProbeOptions{RunID: "transport-mismatch", ProbePath: "/var/tmp/transport-mismatch", RequestedTransport: rootfs.TransportUblk},
	)

	require.Error(t, err)
	require.Equal(t, resumeProbeNotRun, result.Status)
	require.False(t, result.Accepted)
	require.Equal(t, string(rootfs.TransportNBD), result.Transport)
	require.True(t, handles["a"].closed, "the resumed handle must still be cleaned up")
}
func TestRunResumeProbeRetainsCleanupFailureAsFailure(t *testing.T) {
	closeErr := errors.New("close failed")
	handles := map[string]*fakeResumeProbeSandbox{
		"a": {id: "a", transport: rootfs.TransportNBD},
		"b": {id: "b", transport: rootfs.TransportNBD, closeErr: closeErr},
	}
	requests := []resumeProbeRequest{
		{Label: "a", Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "sandbox-a"}},
		{Label: "b", Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "sandbox-b"}},
	}

	result, err := runResumeProbe(
		context.Background(),
		func(_ context.Context, request resumeProbeRequest) (resumeProbeSandbox, error) {
			return handles[request.Label], nil
		},
		requests,
		immutableProbeMarker{Path: "/var/tmp/marker", Content: "marker", SHA256: "digest"},
		resumeProbeOptions{RunID: "cleanup", ProbePath: "/var/tmp/cleanup-probe"},
	)

	require.Error(t, err)
	require.False(t, result.Accepted)
	require.True(t, handles["a"].closed)
	require.True(t, handles["b"].closed)
	require.True(t, strings.Contains(err.Error(), "close sandbox 1"))
}

func TestResumeProbeScriptsExecuteWithBinSh(t *testing.T) {
	dir := t.TempDir()
	immutablePath := filepath.Join(dir, "immutable")
	probePath := filepath.Join(dir, "probe")
	immutableContent := "known-marker"
	writableMarker := "private-token"
	digest := sha256.Sum256([]byte(immutableContent + "\n"))
	marker := immutableProbeMarker{
		Path:    immutablePath,
		Content: immutableContent,
		SHA256:  hex.EncodeToString(digest[:]),
	}
	require.NoError(t, os.WriteFile(immutablePath, []byte(immutableContent+"\n"), 0o644))

	syncBin := filepath.Join(dir, "sync")
	require.NoError(t, os.WriteFile(syncBin, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	run := func(name, script string) {
		cmd := exec.Command("/bin/sh", "-c", script) //nolint:gosec // test supplies generated scripts
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(syncBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
		output, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "%s failed: output=%q script=\n%s", name, output, script)
	}

	run("resume probe write/readback", resumeProbeScript(marker, probePath, writableMarker))
	got, err := os.ReadFile(probePath)
	require.NoError(t, err)
	require.Equal(t, []byte("private-token\nprivate-token-modified\n"), got)

	run("resume probe verify", verifyProbeScript(probePath, writableMarker))
}

func TestResumeProbeScriptRequiresImmutableMarkerAndFreshWritablePath(t *testing.T) {
	script := resumeProbeScript(
		immutableProbeMarker{Path: "/var/tmp/immutable", Content: "content", SHA256: "digest"},
		"/var/tmp/probe",
		"run-sandbox-0",
	)

	require.Contains(t, script, `test "$(cat "$immutable")" = "$expected_content"`)
	require.Contains(t, script, `test "$(sha256sum "$immutable" | cut -d ' ' -f 1)" = "$expected_digest"`)
	require.Contains(t, script, `test ! -e "$probe"`)
	require.Contains(t, script, "run-sandbox-0-modified")

	negativeRead := negativePrivatePathReadScript("/var/tmp/probe", "run-sandbox-0")
	require.Contains(t, negativeRead, `if actual=$(cat "$probe" 2>/dev/null); then`)
	require.Contains(t, negativeRead, "run-sandbox-0")
	require.Contains(t, negativeRead, `test ! -e "$probe"`)
}
