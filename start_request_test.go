package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// fakeProc records what Start hands to the service process, standing in for
// the uvicorn process so these tests need no python, docker or network.
type fakeProc struct {
	envs    []*resources.EnvironmentVariable
	started bool
	stopped bool
}

func (p *fakeProc) Start(context.Context) error             { p.started = true; return nil }
func (p *fakeProc) Run(context.Context) error               { return nil }
func (p *fakeProc) Stop(context.Context) error              { p.stopped = true; return nil }
func (p *fakeProc) Wait(context.Context) error              { return nil }
func (p *fakeProc) IsRunning(context.Context) (bool, error) { return p.started && !p.stopped, nil }
func (p *fakeProc) WaitOn(string)                           {}
func (p *fakeProc) WithDir(string)                          {}
func (p *fakeProc) WithOutput(io.Writer)                    {}
func (p *fakeProc) StdinPipe() (io.WriteCloser, error)      { return nil, errors.New("no stdin") }
func (p *fakeProc) StdoutPipe() (io.ReadCloser, error)      { return nil, errors.New("no stdout") }

func (p *fakeProc) WithEnvironmentVariables(_ context.Context, envs ...*resources.EnvironmentVariable) {
	p.envs = append(p.envs, envs...)
}

func (p *fakeProc) WithEnvironmentVariablesAppend(context.Context, *resources.EnvironmentVariable, string) {
}

type fakeRunnerEnvironment struct {
	mu    sync.Mutex
	procs []*fakeProc
}

func (e *fakeRunnerEnvironment) Init(context.Context) error     { return nil }
func (e *fakeRunnerEnvironment) Stop(context.Context) error     { return nil }
func (e *fakeRunnerEnvironment) Shutdown(context.Context) error { return nil }
func (e *fakeRunnerEnvironment) WithBinary(string) error        { return nil }

func (e *fakeRunnerEnvironment) WithEnvironmentVariables(context.Context, ...*resources.EnvironmentVariable) {
}

func (e *fakeRunnerEnvironment) NewProcess(string, ...string) (runners.Proc, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	proc := &fakeProc{}
	e.procs = append(e.procs, proc)
	return proc, nil
}

func (e *fakeRunnerEnvironment) started() []*fakeProc {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*fakeProc(nil), e.procs...)
}

// startTestRuntime builds a Runtime already past Init: a runner environment is
// in place, so Start goes straight to assembling the process.
func startTestRuntime(t *testing.T) (*Runtime, *fakeRunnerEnvironment) {
	t.Helper()
	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	env := &fakeRunnerEnvironment{}
	runtime.runnerEnvironment = env
	t.Cleanup(runtime.Base.StopWatcher)
	return runtime, env
}

// processEnv returns every value the process received for key, in the order
// they were applied.
func processEnv(proc *fakeProc, key string) []string {
	var values []string
	for _, env := range proc.envs {
		if env.Key == key {
			values = append(values, env.ValueAsString())
		}
	}
	return values
}

func dependencyMapping() *basev0.NetworkMapping {
	return &basev0.NetworkMapping{
		Endpoint: &basev0.Endpoint{Module: "mod", Service: "store", Name: "rest", Api: standards.REST},
		Instances: []*basev0.NetworkInstance{{
			Access:   resources.NewNativeNetworkAccess(),
			Hostname: "localhost",
			Port:     9876,
			Address:  "http://localhost:9876",
		}},
	}
}

// TestStartHonorsStartRequest pins the three StartRequest inputs that shape the
// process environment: `codefly run --set` overrides, the fixture selector, and
// the resolved addresses of the endpoints this service depends on.
func TestStartHonorsStartRequest(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	dependency := dependencyMapping()
	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{
		Overrides:                   map[string]string{"LOG_LEVEL": "debug"},
		Fixture:                     "seed",
		DependenciesNetworkMappings: []*basev0.NetworkMapping{dependency},
	})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Len(t, env.procs, 1)
	proc := env.procs[0]
	require.True(t, proc.started)

	require.Equal(t, []string{"debug"}, processEnv(proc, "LOG_LEVEL"))
	require.Equal(t, []string{"seed"}, processEnv(proc, resources.FixturePrefix))

	dependencyKey := resources.EndpointAsEnvironmentVariableKey(resources.EndpointInformationFromProto(dependency.Endpoint))
	require.Equal(t, []string{"http://localhost:9876"}, processEnv(proc, dependencyKey))
}

// TestStartOverrideBeatsConfiguration pins the precedence between the two
// sources that can name the same variable. The environment manager emits
// overrides ahead of configuration-derived values, so a `--set` aimed at a
// configuration key only takes effect because Start reorders them.
func TestStartOverrideBeatsConfiguration(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	require.NoError(t, runtime.EnvironmentVariables.AddConfigurations(ctx, &basev0.Configuration{
		Origin: "mod/store",
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "postgres",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "url", Value: "from-configuration"}},
		}},
	}))

	key := resources.ServiceConfigurationKeyFromUnique("mod/store", "postgres", "url")
	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{key: "from-override"}})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Len(t, env.procs, 1)

	require.Equal(t, []string{"from-override"}, processEnv(env.procs[0], key))
}

// TestStartHotReloadKeepsProcessOnIdenticalRequest proves the hot-reload
// fast path still avoids pointless restarts when nothing about the request
// changed — uvicorn --reload already covers source edits.
func TestStartHotReloadKeepsProcessOnIdenticalRequest(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)
	runtime.FastAPI.Settings.HotReload = true

	req := &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}}
	_, err := runtime.Start(ctx, req)
	require.NoError(t, err)
	require.Len(t, env.procs, 1)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Len(t, env.procs, 1, "an unchanged request must reuse the running process")
	require.False(t, env.procs[0].stopped)
}

// TestStartHotReloadReplacesProcessOnChangedRequest is the counterpart: a
// running process keeps the environment it was launched with, so new inputs
// only reach the service through a fresh process.
func TestStartHotReloadReplacesProcessOnChangedRequest(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)
	runtime.FastAPI.Settings.HotReload = true

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Len(t, env.procs, 1)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "trace"}})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Len(t, env.procs, 2)
	require.True(t, env.procs[0].stopped, "the process carrying the old environment must be torn down")
	require.Equal(t, []string{"trace"}, processEnv(env.procs[1], "LOG_LEVEL"))
}

// TestStartOverrideBeatsSecret pins the other half of the configuration:
// secret values reach the process through the same All() list as plain ones, so
// a `--set` aimed at a secret key must win rather than be silently shadowed.
func TestStartOverrideBeatsSecret(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	require.NoError(t, runtime.EnvironmentVariables.AddConfigurations(ctx, &basev0.Configuration{
		Origin: "mod/store",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "postgres",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "password", Value: "real-secret", Secret: true},
				{Key: "token", Value: "untouched-secret", Secret: true},
			},
		}},
	}))

	key := resources.ServiceSecretConfigurationKeyFromUnique("mod/store", "postgres", "password")
	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{key: "stub"}})
	require.NoError(t, err)
	require.Len(t, env.procs, 1)

	require.Equal(t, []string{"stub"}, processEnv(env.procs[0], key))
	// Secrets travel inside All(); an untouched one must still reach the process.
	untouched := resources.ServiceSecretConfigurationKeyFromUnique("mod/store", "postgres", "token")
	require.Equal(t, []string{"untouched-secret"}, processEnv(env.procs[0], untouched))
}

// TestStartWithoutOverridesLeavesEnvironmentUnchanged guards the blast radius of
// the override resolution: a service that sets nothing must receive exactly what
// the environment manager produced, entry for entry.
func TestStartWithoutOverridesLeavesEnvironmentUnchanged(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, env.procs, 1)

	expected, err := runtime.EnvironmentVariables.All()
	require.NoError(t, err)
	require.Equal(t, resources.EnvironmentVariableAsStrings(expected), resources.EnvironmentVariableAsStrings(env.procs[0].envs))
}

// TestStartWithdrawnFixtureReverts proves the fixture is not a latch: dropping
// it from a later request must stop exporting it, not leave the previous
// selector in place for fixture-aware app code to branch on.
func TestStartWithdrawnFixtureReverts(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Fixture: "seed"})
	require.NoError(t, err)
	require.Equal(t, []string{"seed"}, processEnv(env.procs[0], resources.FixturePrefix))

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, env.procs, 2)
	require.Empty(t, processEnv(env.procs[1], resources.FixturePrefix), "a withdrawn fixture must not survive into the next process")
}

// TestStartWithdrawnOverrideRevertsToConfiguration is the same guarantee for
// overrides. The environment manager appends them and never forgets, so without
// the withdrawal accounting the process would keep an override the request no
// longer carries instead of falling back to the configuration value.
func TestStartWithdrawnOverrideRevertsToConfiguration(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	require.NoError(t, runtime.EnvironmentVariables.AddConfigurations(ctx, &basev0.Configuration{
		Origin: "mod/store",
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "postgres",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "url", Value: "from-configuration"}},
		}},
	}))
	key := resources.ServiceConfigurationKeyFromUnique("mod/store", "postgres", "url")

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{key: "from-override"}})
	require.NoError(t, err)
	require.Equal(t, []string{"from-override"}, processEnv(env.procs[0], key))

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, env.procs, 2)
	require.Equal(t, []string{"from-configuration"}, processEnv(env.procs[1], key))
}

// TestStartWithdrawnOverrideDisappearsWithoutConfiguration covers the same
// withdrawal when nothing sits underneath: the key must be gone, not retained.
func TestStartWithdrawnOverrideDisappearsWithoutConfiguration(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Equal(t, []string{"debug"}, processEnv(env.procs[0], "LOG_LEVEL"))

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, env.procs, 2)
	require.Empty(t, processEnv(env.procs[1], "LOG_LEVEL"))
}

// TestStartIgnoresSpecsOnlyChange pins that the restart decision reads only the
// fields this agent honors. Specs is not one of them, so a request differing
// only there describes an identical process and must not bounce a healthy one.
func TestStartIgnoresSpecsOnlyChange(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)
	runtime.FastAPI.Settings.HotReload = true

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Len(t, env.procs, 1)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{
		Overrides: map[string]string{"LOG_LEVEL": "debug"},
		Specs:     &basev0.Specs{Fields: map[string]*basev0.SpecValue{"unread": {}}},
	})
	require.NoError(t, err)
	require.Len(t, env.procs, 1, "a Specs-only difference must not restart the process")
	require.False(t, env.procs[0].stopped)
}

// TestStartSerializesConcurrentStarts guards the runner lifecycle: Start and
// Stop are concurrent gRPC handlers, and two overlapping restarts must not each
// stop the same process and leave one replacement orphaned on the bound port.
func TestStartSerializesConcurrentStarts(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)
	runtime.FastAPI.Settings.HotReload = true

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Len(t, env.started(), 1)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, startErr := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "trace"}})
			require.NoError(t, startErr)
		}()
	}
	wg.Wait()

	procs := env.started()
	require.Len(t, procs, 2, "the changed request must produce exactly one replacement process")
	require.True(t, procs[0].stopped)
	require.False(t, procs[1].stopped)
	require.Equal(t, []string{"trace"}, processEnv(procs[1], "LOG_LEVEL"))
}

// TestUnresolvedDependencies pins the diagnostic for dependency addresses that
// no runtime context can reach — AddEndpoints drops those silently, which would
// otherwise surface as a missing variable deep inside the running service.
func TestUnresolvedDependencies(t *testing.T) {
	reachable := dependencyMapping()
	unreachable := dependencyMapping()
	unreachable.Endpoint.Service = "cache"
	unreachable.Instances[0].Access = resources.NewContainerNetworkAccess()

	mappings := []*basev0.NetworkMapping{reachable, unreachable, nil}
	require.Equal(t, []string{"mod/cache/rest"}, unresolvedDependencies(mappings, resources.NewNativeNetworkAccess()))
	require.Empty(t, unresolvedDependencies([]*basev0.NetworkMapping{reachable}, resources.NewNativeNetworkAccess()))
}
