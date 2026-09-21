//go:build linux

package smoketest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/artifact"
	"github.com/e2b-dev/infra/packages/shared/pkg/fcversion"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

func TestSmokeHelpersStageExplicitLocalFixtures(t *testing.T) {
	sourceDir := t.TempDir()
	dataDir := t.TempDir()
	kernelSource := filepath.Join(sourceDir, "vmlinux.bin")
	firecrackerSource := filepath.Join(sourceDir, "firecracker")
	require.NoError(t, os.WriteFile(kernelSource, []byte("kernel-fixture"), 0o600))
	require.NoError(t, os.WriteFile(firecrackerSource, []byte("firecracker-fixture"), 0o600))

	t.Setenv(localKernelFixtureEnv, kernelSource)
	downloadKernel(t, dataDir)
	stagedKernel := filepath.Join(dataDir, "kernels", featureflags.DefaultKernelVersion, artifact.KernelFileName)
	gotKernel, err := os.ReadFile(stagedKernel)
	require.NoError(t, err)
	require.Equal(t, []byte("kernel-fixture"), gotKernel)

	t.Setenv(localFirecrackerFixtureEnv, firecrackerSource)
	downloadFC(t, dataDir, featureflags.DefaultFirecrackerVersion)
	stagedFirecracker := filepath.Join(dataDir, "fc-versions", featureflags.DefaultFirecrackerVersion, artifact.FirecrackerBinaryName)
	if info, err := fcversion.New(featureflags.DefaultFirecrackerVersion); err == nil {
		if _, isE2B := info.E2BVersion(); isE2B {
			stagedFirecracker = filepath.Join(dataDir, "fc-versions", featureflags.DefaultFirecrackerVersion, utils.TargetArch(), artifact.FirecrackerBinaryName)
		}
	}
	gotFirecracker, err := os.ReadFile(stagedFirecracker)
	require.NoError(t, err)
	require.Equal(t, []byte("firecracker-fixture"), gotFirecracker)

	kernelInfo, err := os.Stat(stagedKernel)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), kernelInfo.Mode().Perm())
	firecrackerInfo, err := os.Stat(stagedFirecracker)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), firecrackerInfo.Mode().Perm())
}
