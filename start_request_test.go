package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// fakeProc records what Start hands to the service process, standing in for
// the uvicorn process so these tests need no python, docker or network. It
// models a process that stays up until a test kills it: Wait blocks, which is
// what the runner supervisor expects of a healthy service.
type fakeProc struct {
	bin  string
	args []string
	envs []*resources.EnvironmentVariable

	// exit releases Wait, standing in for the process dying on its own.
	exit    chan struct{}
	exitErr error

	// output is the writer Start wires the process to, so a test can play back
	// what a dying process would have printed.
	output io.Writer

	// mu guards the lifecycle flags: the supervisor probes liveness from its
	// own goroutine while the test reads them.
	mu      sync.Mutex
	started bool
	stopped bool
}

func newFakeProc(bin string, args []string) *fakeProc {
	return &fakeProc{bin: bin, args: args, exit: make(chan struct{})}
}

func (p *fakeProc) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}

func (p *fakeProc) Run(context.Context) error { return nil }

func (p *fakeProc) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		close(p.exit)
	}
	return nil
}

// die stands in for the process going away on its own — an import error, a
// crash, or a clean exit nobody asked for.
func (p *fakeProc) die(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	p.exitErr = err
	close(p.exit)
}

func (p *fakeProc) Wait(ctx context.Context) error {
	select {
	case <-p.exit:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *fakeProc) IsRunning(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started && !p.stopped, nil
}

func (p *fakeProc) isStarted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

func (p *fakeProc) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

func (p *fakeProc) WaitOn(string)                      {}
func (p *fakeProc) WithDir(string)                     {}
func (p *fakeProc) WithOutput(w io.Writer)             { p.output = w }
func (p *fakeProc) StdinPipe() (io.WriteCloser, error) { return nil, errors.New("no stdin") }
func (p *fakeProc) StdoutPipe() (io.ReadCloser, error) { return nil, errors.New("no stdout") }

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

func (e *fakeRunnerEnvironment) NewProcess(bin string, args ...string) (runners.Proc, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	proc := newFakeProc(bin, args)
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
	// Load is what wires the service logger the process output is forwarded to;
	// these tests skip straight to Start, so stand it up here.
	runtime.Logger = runtime.Wool
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
	require.True(t, proc.isStarted())

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
	require.False(t, env.procs[0].isStopped())
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
	require.True(t, env.procs[0].isStopped(), "the process carrying the old environment must be torn down")
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
	require.False(t, env.procs[0].isStopped())
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
	require.True(t, procs[0].isStopped())
	require.False(t, procs[1].isStopped())
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

// currentRunner reads the live generation the way the runtime's own code does.
// The supervisor goroutine replaces it under the same mutex.
func currentRunner(runtime *Runtime) *runnerHandle {
	runtime.runnerMu.Lock()
	defer runtime.runnerMu.Unlock()
	return runtime.runner
}

// startStatus reads the status the orchestrator polls for, which is the only
// channel a dead runner has to reach `codefly run`.
func startStatus(t *testing.T, runtime *Runtime) *runtimev0.StartStatus {
	t.Helper()
	info, err := runtime.Base.Runtime.InformationResponse(context.Background(), &runtimev0.InformationRequest{})
	require.NoError(t, err)
	return info.GetStartStatus()
}

// TestUvicornArgsReloadIsConditional pins the command line: --reload makes
// uvicorn fork a supervisor that re-executes the app on source edits, which a
// service configured without hot reload never asked for.
func TestUvicornArgsReloadIsConditional(t *testing.T) {
	require.Equal(t,
		[]string{"run", "uvicorn", "src.main:app", "--host", "0.0.0.0", "--port", "8080"},
		uvicornArgs(8080, false))
	require.Equal(t,
		[]string{"run", "uvicorn", "src.main:app", "--host", "0.0.0.0", "--port", "8080", "--reload"},
		uvicornArgs(8080, true))
}

// TestStartWithoutHotReloadOmitsReloadFlag is the same guarantee seen from
// Start, where the setting is actually read.
func TestStartWithoutHotReloadOmitsReloadFlag(t *testing.T) {
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, env.procs, 1)
	require.Equal(t, "uv", env.procs[0].bin)
	require.NotContains(t, env.procs[0].args, "--reload")
}

// TestStartWithoutHotReloadIsIdempotent covers the setting that used to skip
// the reuse check entirely: a second identical Start built a second process and
// overwrote the handle to the first, leaving it running and unreachable.
func TestStartWithoutHotReloadIsIdempotent(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Len(t, env.started(), 1)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Len(t, env.started(), 1, "an unchanged request must reuse the running process")
	require.False(t, env.procs[0].isStopped())
}

// TestStartWithoutHotReloadReplacesProcessOnChangedRequest is the counterpart:
// new inputs still get a new process, and the one carrying the old environment
// is reaped rather than left behind on the bound port.
func TestStartWithoutHotReloadReplacesProcessOnChangedRequest(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "trace"}})
	require.NoError(t, err)

	procs := env.started()
	require.Len(t, procs, 2)
	require.True(t, procs[0].isStopped(), "the process carrying the old environment must be torn down")
	require.False(t, procs[1].isStopped())
	require.Equal(t, []string{"trace"}, processEnv(procs[1], "LOG_LEVEL"))
}

// TestStartRestartsDeadProcess covers the shortcut that used to answer STARTED
// from a non-nil handle: a process that died has to be replaced, not reported.
func TestStartRestartsDeadProcess(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)
	runtime.FastAPI.Settings.HotReload = true

	req := &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}}
	_, err := runtime.Start(ctx, req)
	require.NoError(t, err)
	require.Len(t, env.started(), 1)

	env.procs[0].die(errors.New("exit status 1"))

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	procs := env.started()
	require.Len(t, procs, 2, "an unchanged request against a dead process must start a new one")
	require.False(t, procs[1].isStopped())
	require.Equal(t, []string{"debug"}, processEnv(procs[1], "LOG_LEVEL"))
}

// TestRunnerExitRevokesStarted is the reason the supervisor exists: a process
// that dies after Start committed STARTED has to move the status back, since
// polling Information is all the orchestrator has to learn about it. The
// diagnostic must carry what the process printed on its way out — the exit
// status alone does not say which import failed.
func TestRunnerExitRevokesStarted(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, startStatus(t, runtime).GetState())

	proc := env.procs[0]
	_, err = proc.output.Write([]byte("ImportError: no module named 'missing'\n"))
	require.NoError(t, err)
	proc.die(errors.New("exit status 1"))

	require.Eventually(t, func() bool {
		return startStatus(t, runtime).GetState() == runtimev0.StartStatus_ERROR
	}, time.Second, 5*time.Millisecond)

	message := startStatus(t, runtime).GetMessage()
	require.Contains(t, message, "exit status 1")
	require.Contains(t, message, "ImportError: no module named 'missing'")
}

// TestCleanRunnerExitRevokesStarted covers the other half: uvicorn returning
// zero without anybody asking it to stop is still a service that is gone.
func TestCleanRunnerExitRevokesStarted(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	env.procs[0].die(nil)

	require.Eventually(t, func() bool {
		return startStatus(t, runtime).GetState() == runtimev0.StartStatus_ERROR
	}, time.Second, 5*time.Millisecond)
	require.Contains(t, startStatus(t, runtime).GetMessage(), "exited unexpectedly")
}

// TestStopDoesNotRevokeStarted pins the other side of the same signal: a stop
// we asked for is not a crash, and must not be reported as one.
func TestStopDoesNotRevokeStarted(t *testing.T) {
	ctx := context.Background()
	runtime, _ := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)

	require.Never(t, func() bool {
		return startStatus(t, runtime).GetState() == runtimev0.StartStatus_ERROR
	}, 200*time.Millisecond, 10*time.Millisecond)
}

// TestStaleRunnerExitLeavesReplacementStarted pins the generation guard. A
// supervisor can reach the mutex after the Start that replaced its process has
// already committed STARTED; the dead generation must not revoke a status that
// now describes a different, running process.
func TestStaleRunnerExitLeavesReplacementStarted(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}})
	require.NoError(t, err)
	stale := currentRunner(runtime)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "trace"}})
	require.NoError(t, err)
	require.Len(t, env.started(), 2)

	runtime.reportRunnerExit(stale, errors.New("exit status 1"))

	require.Equal(t, runtimev0.StartStatus_STARTED, startStatus(t, runtime).GetState())
	require.NotNil(t, currentRunner(runtime), "the replacement must still own the service")
}

// TestConcurrentStartStopAndExits runs the whole lifecycle against itself: the
// value here is the race detector, not the assertions.
func TestConcurrentStartStopAndExits(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)
	runtime.FastAPI.Settings.HotReload = true

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, startErr := runtime.Start(ctx, &runtimev0.StartRequest{
				Overrides: map[string]string{"LOG_LEVEL": fmt.Sprintf("level-%d", i%3)},
			})
			require.NoError(t, startErr)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, proc := range env.started() {
			proc.die(errors.New("exit status 1"))
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, stopErr := runtime.Stop(ctx, &runtimev0.StopRequest{})
		require.NoError(t, stopErr)
	}()
	wg.Wait()

	_, err := runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Nil(t, currentRunner(runtime))
}

// TestTailWriterKeepsRecentLines pins the bounded ring the exit diagnostic is
// built from: the tail of a traceback is what identifies the failure, and it
// must survive however the process chunked its output.
func TestTailWriterKeepsRecentLines(t *testing.T) {
	writer := &tailWriter{max: 3}
	for i := range 5 {
		_, err := fmt.Fprintf(writer, "line-%d\n", i)
		require.NoError(t, err)
	}
	require.Equal(t, "line-2\nline-3\nline-4", writer.Tail())

	split := &tailWriter{max: 3}
	_, err := split.Write([]byte("Import"))
	require.NoError(t, err)
	_, err = split.Write([]byte("Error: boom\n\n"))
	require.NoError(t, err)
	require.Equal(t, "ImportError: boom", split.Tail())

	unterminated := &tailWriter{max: 3}
	_, err = unterminated.Write([]byte("dying words"))
	require.NoError(t, err)
	require.Equal(t, "dying words", unterminated.Tail())
}

// TestStartRelaunchesWhenInitRepublishesThePort pins that the restart decision
// covers the ports Init owns, not just the StartRequest. An Init between two
// identical Starts can move the service's port; the process bound to the old
// one is not what the caller is being told is running.
func TestStartRelaunchesWhenInitRepublishesThePort(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	runtime.port = 9000
	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Len(t, env.started(), 1)
	require.Contains(t, env.started()[0].args, "9000")

	// What a re-Init resolving a different network mapping publishes.
	runtime.port = 9100
	resp, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	procs := env.started()
	require.Len(t, procs, 2, "the process on the superseded port must be replaced")
	require.True(t, procs[0].isStopped(), "the process on the old port must be stopped")
	require.Contains(t, procs[1].args, "9100")
}

// TestStartDoesNotRefoldAnUnchangedRequest pins the other half: the port change
// above must not be mistaken for a request change. The environment manager
// appends overrides and never removes them, so re-folding an identical request
// would duplicate every entry it carries.
func TestStartDoesNotRefoldAnUnchangedRequest(t *testing.T) {
	ctx := context.Background()
	runtime, env := startTestRuntime(t)

	req := &runtimev0.StartRequest{Overrides: map[string]string{"LOG_LEVEL": "debug"}}
	runtime.port = 9000
	_, err := runtime.Start(ctx, req)
	require.NoError(t, err)

	runtime.port = 9100
	_, err = runtime.Start(ctx, req)
	require.NoError(t, err)

	procs := env.started()
	require.Len(t, procs, 2)
	require.Equal(t, []string{"debug"}, processEnv(procs[1], "LOG_LEVEL"),
		"an unchanged request must be folded into the environment manager exactly once")
	require.Equal(t, 1, runtime.appliedOverrides["LOG_LEVEL"])
}

// TestStartRefusesAnAbandonedRequest pins that Start does not launch a process
// for a caller that has already given up. Start can wait on initMu for as long
// as an Init takes — a docker pull, a `uv sync` — and the process it launches
// runs on a context of its own, so it would outlive the request entirely.
func TestStartRefusesAnAbandonedRequest(t *testing.T) {
	runtime, env := startTestRuntime(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
	require.Empty(t, env.started(), "no process may be launched for an abandoned request")
	require.Nil(t, currentRunner(runtime))
}
