package main

import (
	"context"
	"io"
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
func (p *fakeProc) StdinPipe() (io.WriteCloser, error)      { return nil, nil }
func (p *fakeProc) StdoutPipe() (io.ReadCloser, error)      { return nil, nil }

func (p *fakeProc) WithEnvironmentVariables(_ context.Context, envs ...*resources.EnvironmentVariable) {
	p.envs = append(p.envs, envs...)
}

func (p *fakeProc) WithEnvironmentVariablesAppend(context.Context, *resources.EnvironmentVariable, string) {
}

type fakeRunnerEnvironment struct {
	procs []*fakeProc
}

func (e *fakeRunnerEnvironment) Init(context.Context) error     { return nil }
func (e *fakeRunnerEnvironment) Stop(context.Context) error     { return nil }
func (e *fakeRunnerEnvironment) Shutdown(context.Context) error { return nil }
func (e *fakeRunnerEnvironment) WithBinary(string) error        { return nil }

func (e *fakeRunnerEnvironment) WithEnvironmentVariables(context.Context, ...*resources.EnvironmentVariable) {
}

func (e *fakeRunnerEnvironment) NewProcess(string, ...string) (runners.Proc, error) {
	proc := &fakeProc{}
	e.procs = append(e.procs, proc)
	return proc, nil
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
