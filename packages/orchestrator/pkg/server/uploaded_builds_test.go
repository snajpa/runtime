//go:build linux

package server

import (
	"testing"

	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// TestUploadedBuildsHintOutlivesTemplateCacheWindow pins the lifetime of the
// node-local uploaded-build hint: a shorter hint silently re-enables peer chunk
// serving for builds that are already in object storage (S-38).
func TestUploadedBuildsHintOutlivesTemplateCacheWindow(t *testing.T) {
	t.Parallel()

	require.GreaterOrEqual(t, uploadedBuildsTTL, template.CacheExpiration)
}

// TestUploadedBuildsHintAnswersUseStorage documents the hint's contract: while
// a build is marked uploaded, chunk requests are answered "use storage" without
// touching the peer path. REQ-G4: absence stays the safe fallback.
func TestUploadedBuildsHintAnswersUseStorage(t *testing.T) {
	t.Parallel()

	s := &Server{
		uploadedBuilds: ttlcache.New[string, struct{}](
			ttlcache.WithTTL[string, struct{}](uploadedBuildsTTL),
		),
	}
	go s.uploadedBuilds.Start()
	t.Cleanup(s.uploadedBuilds.Stop)

	s.uploadedBuilds.Set("build-1", struct{}{}, ttlcache.DefaultTTL)

	resp, err := s.GetBuildFileExists(t.Context(), &orchestrator.GetBuildFileExistsRequest{
		BuildId: "build-1",
		Name:    "memfile",
	})
	require.NoError(t, err)
	require.True(t, resp.GetAvailability().GetUseStorage())
}
