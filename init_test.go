package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

const probeProject = `[project]
name = "init-probe"
version = "0.0.0"
`

// initTestRuntime stands a Runtime up at the point Init is called from: a
// native runtime, a source tree holding the files Init hashes, and the REST
// endpoint the proposed mappings resolve.
func initTestRuntime(t *testing.T) (*Runtime, *runtimev0.InitRequest) {
	t.Helper()

	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	runtime.Logger = runtime.Wool
	// Load is what normally supplies these; these tests call Init directly.
	runtime.Identity = resources.ServiceIdentityFromProto(&basev0.ServiceIdentity{
		Name:          "probe",
		Module:        "mod",
		Workspace:     "workspace",
		WorkspacePath: runtime.Location,
	})
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextNative()

	source := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(source, "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "pyproject.toml"), []byte(probeProject), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "main.py"), []byte("app = None\n"), 0o600))
	runtime.Service.SourceLocation = source

	endpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "rest", Api: standards.REST}
	runtime.FastAPI.RestEndpoint = endpoint
	t.Cleanup(runtime.Base.StopWatcher)

	return runtime, &runtimev0.InitRequest{
		RuntimeContext: resources.NewRuntimeContextNative(),
		ProposedNetworkMappings: []*basev0.NetworkMapping{{
			Endpoint: endpoint,
			Instances: []*basev0.NetworkInstance{{
				Access:   resources.NewNativeNetworkAccess(),
				Hostname: "localhost",
				Port:     9999,
				Address:  "http://localhost:9999",
			}},
		}},
	}
}

// releasingEnv releases the runtime's runner state the moment Init hands it
// control, standing in for a Stop or Destroy landing in the middle of Init's
// runner section.
type releasingEnv struct {
	*fakeRunnerEnvironment
	runtime *Runtime
}

func (e *releasingEnv) Init(context.Context) error {
	e.runtime.runnerMu.Lock()
	defer e.runtime.runnerMu.Unlock()
	e.runtime.runnerEnvironment = nil
	e.runtime.cacheLocation = ""
	return nil
}

// TestInitWorksAgainstTheEnvironmentItCaptured is the contract: Init reads the
// environment and its cache location once and then works against those, so a
// teardown replacing the fields underneath it cannot leave Init dereferencing
// a nil environment or caching against nothing.
func TestInitWorksAgainstTheEnvironmentItCaptured(t *testing.T) {
	ctx := context.Background()
	runtime, req := initTestRuntime(t)

	env := &releasingEnv{fakeRunnerEnvironment: &fakeRunnerEnvironment{}, runtime: runtime}
	cache := t.TempDir()
	runtime.runnerEnvironment = env
	runtime.cacheLocation = cache

	resp, err := runtime.Init(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	require.Len(t, env.started(), 2, "uv sync and the OpenAPI generation must run on the captured environment")

	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the dependency hashes must land in the captured cache location")
}

// TestInitIsSafeWithConcurrentTeardown overlaps Init with the calls that own
// the same fields. Under `-race` this is what catches Init publishing or
// reading the runner state outside runnerMu; without it, it still pins that
// neither side panics however they interleave.
func TestInitIsSafeWithConcurrentTeardown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		teardown func(*Runtime)
	}{
		{"destroy", func(r *Runtime) { _, _ = r.Destroy(context.Background(), &runtimev0.DestroyRequest{}) }},
		{"stop", func(r *Runtime) { _, _ = r.Stop(context.Background(), &runtimev0.StopRequest{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, req := initTestRuntime(t)
			// Init creates and publishes the runner environment — the section
			// under test — and then stops on the missing project file, well
			// before it would reach for uv.
			require.NoError(t, os.Remove(filepath.Join(runtime.Service.SourceLocation, "pyproject.toml")))

			gate := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-gate
				_, _ = runtime.Init(context.Background(), req)
			}()
			go func() {
				defer wg.Done()
				<-gate
				tc.teardown(runtime)
			}()
			close(gate)
			wg.Wait()

			require.DirExists(t, filepath.Join(runtime.Location, ".cache/local"),
				"Init must have reached its runner section, or this tests nothing")
		})
	}
}
