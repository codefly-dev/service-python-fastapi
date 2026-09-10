package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

const probeProject = `[project]
name = "init-probe"
version = "0.0.0"
`

// isolateWorkingDir moves the test off the checkout. builders.Dependencies
// resolves its hash file relative to the cache location, so a regression that
// empties that location writes uv.hash into the working directory.
func isolateWorkingDir(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

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
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, req := initTestRuntime(t)

	env := &releasingEnv{fakeRunnerEnvironment: &fakeRunnerEnvironment{}, runtime: runtime}
	cache := t.TempDir()
	runtime.runnerEnvironment = env
	runtime.cacheLocation = cache

	resp, err := runtime.Init(ctx, req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_READY, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	var commands [][]string
	for _, proc := range env.started() {
		commands = append(commands, append([]string{proc.bin}, proc.args...))
	}
	require.Equal(t, [][]string{
		{"uv", "sync"},
		{"uv", "run", "python", "src/openapi.py"},
	}, commands, "both must run on the captured environment")

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
		name string
		// teardown reports whether the call returned a response: a panic is
		// swallowed by the deferred Wool.Catch and surfaces as a nil one.
		teardown func(*Runtime) bool
	}{
		{"destroy", func(r *Runtime) bool {
			resp, err := r.Destroy(context.Background(), &runtimev0.DestroyRequest{})
			return err == nil && resp != nil
		}},
		{"stop", func(r *Runtime) bool {
			resp, err := r.Stop(context.Background(), &runtimev0.StopRequest{})
			return err == nil && resp != nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateWorkingDir(t)
			// One interleaving proves little: the race detector reports only
			// accesses that actually execute, and a teardown landing before
			// Init publishes touches none of the runner state. Repeat so both
			// orders occur.
			for range 50 {
				runInitAgainstTeardown(t, tc.teardown)
			}
		})
	}
}

func runInitAgainstTeardown(t *testing.T, teardown func(*Runtime) bool) {
	t.Helper()
	runtime, req := initTestRuntime(t)
	// Init creates and publishes the runner environment — the section under
	// test — and then stops on the missing project file, well before it would
	// reach for uv.
	require.NoError(t, os.Remove(filepath.Join(runtime.Service.SourceLocation, "pyproject.toml")))

	var initResp *runtimev0.InitResponse
	var initErr error
	var teardownOK bool

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-gate
		initResp, initErr = runtime.Init(context.Background(), req)
	}()
	go func() {
		defer wg.Done()
		<-gate
		// Land in the window Init is actually vulnerable in. Racing the two
		// from a bare start gate is not enough: the teardown wins, finds no
		// environment, and touches none of the runner state — so the accesses
		// that would race never execute and the detector sees nothing.
		waitForRunnerEnvironment(runtime)
		teardownOK = teardown(runtime)
	}()
	close(gate)
	wg.Wait()

	require.NoError(t, initErr)
	require.NotNil(t, initResp, "Init must not panic when a teardown lands mid-flight")
	require.Equal(t, runtimev0.InitStatus_ERROR, initResp.GetStatus().GetState())
	require.Contains(t, initResp.GetStatus().GetMessage(), "no pyproject.toml",
		"Init must run past its runner section, or this tests nothing")
	require.True(t, teardownOK, "the teardown must not panic against a concurrent Init")
	require.DirExists(t, filepath.Join(runtime.Location, ".cache/local"),
		"Init must have created and published the runner environment")
}

// waitForRunnerEnvironment blocks until Init has published the environment.
// The read is taken under runnerMu, so waiting introduces no edge of its own
// between Init and the teardown.
func waitForRunnerEnvironment(runtime *Runtime) {
	for range 2000 {
		runtime.runnerMu.Lock()
		published := runtime.runnerEnvironment != nil
		runtime.runnerMu.Unlock()
		if published {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunnerEnvCreatesOneEnvironment pins the single-flight: concurrent Inits
// on a cold runtime must share one environment. Without it each caller builds
// its own, one wins the field, and the losers are left initialized — holding a
// docker client and a log stream — with nothing left to shut them down.
func TestRunnerEnvCreatesOneEnvironment(t *testing.T) {
	isolateWorkingDir(t)
	runtime, _ := initTestRuntime(t)

	const callers = 8
	envs := make([]runners.RunnerEnvironment, callers)
	errs := make([]error, callers)

	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			envs[i], _, errs[i] = runtime.runnerEnv(context.Background())
		}()
	}
	close(gate)
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		require.NotNil(t, envs[i])
		require.True(t, envs[i] == envs[0], "every caller must get the one environment that was published")
	}
}

// TestInitAfterStopBuildsAFreshEnvironment pins what Stop owes the next Init:
// Shutdown closes the docker client, so an environment left in the field after
// it cannot serve another process.
func TestInitAfterStopBuildsAFreshEnvironment(t *testing.T) {
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, _ := initTestRuntime(t)

	first, _, err := runtime.runnerEnv(ctx)
	require.NoError(t, err)
	require.NotNil(t, first)

	resp, err := runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StopStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Nil(t, runtime.FastAPI.Service.ActiveEnv(), "Code and the REPL must not be left holding a dead environment")

	second, _, err := runtime.runnerEnv(ctx)
	require.NoError(t, err)
	require.False(t, second == first, "Stop shut the environment down; Init must not be handed the corpse")
}
