package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
)

// initStartTestRuntime builds on initTestRuntime and adds what an Init/Start
// overlap needs: a runner environment Start can launch against without a real
// uv, a cache location for the dependency hashes, and a configuration, so Init
// still has environment-manager work to do after its runner section — the
// window a Start can land in.
func initStartTestRuntime(t *testing.T) (*Runtime, *fakeRunnerEnvironment, *runtimev0.InitRequest) {
	t.Helper()

	runtime, req := initTestRuntime(t)
	runtime.cacheLocation = t.TempDir()
	env := &fakeRunnerEnvironment{}
	runtime.runnerEnvironment = env
	req.Configuration = &basev0.Configuration{
		Origin: "mod/probe",
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "postgres",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "url", Value: "postgres://localhost"}},
		}},
	}
	return runtime, env, req
}

// uvicornProc returns the process Start launched, told apart from the ones Init
// runs on the same environment by the binary it invokes.
func uvicornProc(t *testing.T, env *fakeRunnerEnvironment) *fakeProc {
	t.Helper()
	for _, proc := range env.started() {
		if slices.Contains(proc.args, "uvicorn") {
			return proc
		}
	}
	t.Fatal("Start never launched uvicorn")
	return nil
}

// uvicornPort returns the port Start bound the process to.
func uvicornPort(t *testing.T, env *fakeRunnerEnvironment) string {
	t.Helper()
	args := uvicornProc(t, env).args
	i := slices.Index(args, "--port")
	require.GreaterOrEqual(t, i, 0, "uvicorn was launched without a port: %v", args)
	require.Less(t, i+1, len(args), "uvicorn was launched with a bare --port: %v", args)
	return args[i+1]
}

// gatingEnv holds Init inside its runner section until the test releases it, so
// a Start can be issued while Init is demonstrably mid-flight.
type gatingEnv struct {
	*fakeRunnerEnvironment
	entered chan struct{}
	release chan struct{}
}

func (e *gatingEnv) Init(context.Context) error {
	close(e.entered)
	<-e.release
	return nil
}

// TestStartWaitsForInitToPublishItsPorts is the contract: Init publishes the
// port Start binds uvicorn to, so a Start landing in the middle of an Init runs
// after it and launches on the published port rather than on whatever the field
// happened to hold.
//
// It is also where `-race` sees the two calls overlap on the state Init
// publishes. Gating Init inside its runner section is what makes that reliable:
// released from a bare start gate the two bodies order themselves through
// runnerMu, and the detector reports nothing at all.
func TestStartWaitsForInitToPublishItsPorts(t *testing.T) {
	for range 20 {
		runInitAgainstStart(t)
	}
}

func runInitAgainstStart(t *testing.T) {
	t.Helper()
	runtime, fake, req := initStartTestRuntime(t)
	env := &gatingEnv{fakeRunnerEnvironment: fake, entered: make(chan struct{}), release: make(chan struct{})}
	runtime.runnerEnvironment = env

	var wg sync.WaitGroup
	var initResp *runtimev0.InitResponse
	var initErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		initResp, initErr = runtime.Init(context.Background(), req)
	}()

	<-env.entered

	startDone := make(chan struct{})
	var startResp *runtimev0.StartResponse
	var startErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		startResp, startErr = runtime.Start(context.Background(), &runtimev0.StartRequest{})
		close(startDone)
	}()

	// Nothing observable says the Start goroutine reached the lock, so give it
	// the room an unserialized Start would need. Observing startDone is only
	// safe on the failing side: receiving from it would order the Start
	// goroutine ahead of the release below and hide the very race this test
	// exists to surface, so the good path must fall through the timeout having
	// synchronized with nothing.
	select {
	case <-startDone:
		t.Fatal("Start ran to completion while Init was still inside its runner section")
	case <-time.After(50 * time.Millisecond):
	}
	close(env.release)
	wg.Wait()

	require.NoError(t, initErr)
	require.Equal(t, runtimev0.InitStatus_READY, initResp.GetStatus().GetState(), initResp.GetStatus().GetMessage())
	require.NoError(t, startErr)
	require.Equal(t, runtimev0.StartStatus_STARTED, startResp.GetStatus().GetState(), startResp.GetStatus().GetMessage())
	require.Equal(t, "9999", uvicornPort(t, fake))
}

// TestInitIsSafeWithConcurrentStartOnAColdRuntime covers the path the gated
// test cannot reach: the first Init, which runs CreateRunnerEnvironment. That
// function reads the network mappings and the whole environment manager, and
// publishes ActiveEnv — none of it exercised once a runner environment is
// already in place.
//
// Start is pointed at a source location that does not exist, so it does all of
// its reading and then fails in exec rather than launching a real uvicorn.
func TestInitIsSafeWithConcurrentStartOnAColdRuntime(t *testing.T) {
	isolateWorkingDir(t)
	for range 20 {
		runColdInitAgainstStart(t)
	}
}

func runColdInitAgainstStart(t *testing.T) {
	t.Helper()
	runtime, req := initTestRuntime(t)
	// Init creates and publishes the runner environment — the section under
	// test — and then stops on the missing project file, well before uv.
	require.NoError(t, os.Remove(filepath.Join(runtime.Service.SourceLocation, "pyproject.toml")))
	runtime.Service.SourceLocation = filepath.Join(runtime.Service.SourceLocation, "does-not-exist")

	var initResp *runtimev0.InitResponse
	var initErr error
	var startResp *runtimev0.StartResponse

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
		// Land in the window Init is vulnerable in. Before the environment is
		// published Start refuses at once, having read none of the state Init
		// is writing, and the accesses that would race never execute.
		waitForRunnerEnvironment(runtime)
		startResp, _ = runtime.Start(context.Background(), &runtimev0.StartRequest{})
	}()
	close(gate)
	wg.Wait()

	require.NoError(t, initErr)
	require.NotNil(t, initResp, "Init must not panic when a Start lands mid-flight")
	require.Contains(t, initResp.GetStatus().GetMessage(), "no pyproject.toml",
		"Init must run past its runner section, or this tests nothing")
	require.NotNil(t, startResp, "Start must not panic against a concurrent Init")
	require.DirExists(t, filepath.Join(runtime.Location, ".cache/local"),
		"Init must have created and published the runner environment")
}
