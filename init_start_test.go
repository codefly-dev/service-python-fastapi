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
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

const initStartProject = `[project]
name = "init-start-probe"
version = "0.0.0"
`

// initStartTestRuntime stands a Runtime up at the point Init is called from,
// with a runner environment already in place so Init runs its publishing
// section — runtime context, network mappings, ports, environment variables —
// without building a real one.
func initStartTestRuntime(t *testing.T) (*Runtime, *fakeRunnerEnvironment, *runtimev0.InitRequest) {
	t.Helper()

	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	// Load is what supplies these; these tests call Init and Start directly.
	runtime.Logger = runtime.Wool
	runtime.Identity = resources.ServiceIdentityFromProto(&basev0.ServiceIdentity{
		Name:          "probe",
		Module:        "mod",
		Workspace:     "workspace",
		WorkspacePath: runtime.Location,
	})

	source := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(source, "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "pyproject.toml"), []byte(initStartProject), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "main.py"), []byte("app = None\n"), 0o600))
	runtime.Service.SourceLocation = source
	runtime.cacheLocation = t.TempDir()

	endpoint := &basev0.Endpoint{Module: "mod", Service: "probe", Name: "rest", Api: standards.REST}
	runtime.FastAPI.RestEndpoint = endpoint

	env := &fakeRunnerEnvironment{}
	runtime.runnerEnvironment = env
	t.Cleanup(runtime.Base.StopWatcher)

	return runtime, env, &runtimev0.InitRequest{
		RuntimeContext: resources.NewRuntimeContextNative(),
		// A configuration leaves Init environment-manager work to do after its
		// runner section, which is where a Start can overlap it.
		Configuration: &basev0.Configuration{
			Origin: "mod/probe",
			Infos: []*basev0.ConfigurationInformation{{
				Name:                "postgres",
				ConfigurationValues: []*basev0.ConfigurationValue{{Key: "url", Value: "postgres://localhost"}},
			}},
		},
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
func TestStartWaitsForInitToPublishItsPorts(t *testing.T) {
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

	var startResp *runtimev0.StartResponse
	var startErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		startResp, startErr = runtime.Start(context.Background(), &runtimev0.StartRequest{})
	}()

	// Nothing observable says the Start goroutine reached the lock, so give it
	// the room an unserialized Start would need to launch on the unwritten port.
	time.Sleep(100 * time.Millisecond)
	close(env.release)
	wg.Wait()

	require.NoError(t, initErr)
	require.Equal(t, runtimev0.InitStatus_READY, initResp.GetStatus().GetState(), initResp.GetStatus().GetMessage())
	require.NoError(t, startErr)
	require.Equal(t, runtimev0.StartStatus_STARTED, startResp.GetStatus().GetState(), startResp.GetStatus().GetMessage())

	proc := uvicornProc(t, fake)
	require.Contains(t, proc.args, "--port")
	require.Equal(t, "9999", proc.args[slices.Index(proc.args, "--port")+1])
}

// TestInitIsSafeWithConcurrentStart overlaps the two calls that own the same
// fields. Under `-race` this is what catches Init publishing the ports, the
// network mappings, the runtime context and the environment-variable manager
// outside the lock Start reads them under; without it, it still pins that
// neither side panics however they interleave.
func TestInitIsSafeWithConcurrentStart(t *testing.T) {
	runtime, env, req := initStartTestRuntime(t)

	gate := make(chan struct{})
	var wg sync.WaitGroup
	var initErr, startErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-gate
		_, initErr = runtime.Init(context.Background(), req)
	}()
	go func() {
		defer wg.Done()
		<-gate
		_, startErr = runtime.Start(context.Background(), &runtimev0.StartRequest{})
	}()
	close(gate)
	wg.Wait()

	require.NoError(t, initErr)
	require.NoError(t, startErr)
	uvicornProc(t, env)
}
