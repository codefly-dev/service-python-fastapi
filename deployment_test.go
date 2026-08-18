package main

import (
	"os"
	"path/filepath"
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
	"github.com/stretchr/testify/require"
)

func TestDeploymentTemplates(t *testing.T) {
	destination := agenttesting.AssertKustomizeTemplates(t, deploymentFS, Parameters{})

	rendered, err := os.ReadFile(filepath.Join(destination, "base", "deployment.yaml"))
	require.NoError(t, err)
	manifest := string(rendered)

	// Kubernetes rejects runAsNonRoot when the image user is non-numeric, so the
	// workload must pin a numeric UID matching the Dockerfile's appuser (1000).
	require.Contains(t, manifest, "runAsUser: 1000")
	// readOnlyRootFilesystem leaves the container without writable storage, so a
	// scratch mount at /tmp is required for the app to run.
	require.Contains(t, manifest, "mountPath: /tmp")
	// The scratch volume is bounded so a runaway writer evicts only this pod
	// rather than filling node ephemeral storage.
	require.Contains(t, manifest, "sizeLimit: 1Gi")
	// $HOME must point at the writable mount so ~/.cache writes don't hit the
	// read-only root filesystem.
	require.Contains(t, manifest, "value: /tmp")
}
