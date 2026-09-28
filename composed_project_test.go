package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// TestContainerEnvironmentCarriesAProjectOutsideTheWorkspace pins the mount a
// composed service depends on.
//
// A service the user authored lives in the workspace tree; a service a
// workspace *composes* does not — it is resolved into the module cache, or to
// wherever an overlay points. Both are outside the workspace path, and every
// process this environment runs is given the project directory as its working
// directory. Mounting only the workspace therefore produced a container whose
// working directory did not exist inside it: "chdir to cwd ... no such file or
// directory", surfacing as `uv sync` exiting 127 before the service ever
// started.
func TestContainerEnvironmentCarriesAProjectOutsideTheWorkspace(t *testing.T) {
	ownTestContainers(t)
	ctx := context.Background()

	// Docker on macOS only bind-mounts shared paths, and the per-user temp dir
	// is not one — keep both trees inside the repo, as the other container
	// tests do.
	workspace, err := os.MkdirTemp("testdata", "composed-workspace-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	workspace = shared.MustSolvePath(workspace)

	source, err := os.MkdirTemp("testdata", "composed-project-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(source) })
	source = shared.MustSolvePath(source)
	require.NoError(t, os.MkdirAll(filepath.Join(source, "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "pyproject.toml"), []byte(probeProject), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "main.py"), []byte("app = None\n"), 0o600))

	runtime := NewRuntime(NewService())
	runtime.Location = workspace
	runtime.Logger = runtime.Wool
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextContainer()
	runtime.Identity = &resources.ServiceIdentity{
		Name:          fmt.Sprintf("composed-%d", time.Now().UnixMilli()),
		Module:        "mod",
		Workspace:     "test",
		WorkspacePath: workspace,
	}
	runtime.Environment = shared.Must(resources.LocalEnvironment().Proto())
	runtime.Service.SourceLocation = source

	endpoint := &basev0.Endpoint{Module: "mod", Service: runtime.Identity.Name, Name: "rest", Api: standards.REST}
	runtime.FastAPI.RestEndpoint = endpoint
	runtime.NetworkMappings = []*basev0.NetworkMapping{{
		Endpoint: endpoint,
		Instances: []*basev0.NetworkInstance{{
			Access:   resources.NewNativeNetworkAccess(),
			Hostname: "localhost",
			Port:     9998,
			Address:  "http://localhost:9998",
		}},
	}}

	require.False(t, within(workspace, source),
		"the fixture must place the project outside the workspace, as a composed module is")

	env, _, err := runtime.CreateRunnerEnvironment(ctx)
	if err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	t.Cleanup(func() { _ = env.Shutdown(context.Background()) })
	require.NoError(t, env.Init(ctx))

	proc, err := env.NewProcess("sh", "-c", "test -f pyproject.toml")
	require.NoError(t, err)
	proc.WithDir(source)
	require.NoError(t, proc.Run(ctx),
		"the composed project must exist at its own path inside the container")
}
