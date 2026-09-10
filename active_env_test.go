package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	"github.com/stretchr/testify/require"
)

// activeEnvRuntime is a native-mode Runtime ready to build a runner
// environment: Location for the cache directory, a runtime context, and a
// Python source tree for Code to analyze.
func activeEnvRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()

	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	runtime.Logger = runtime.Wool
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextNative()
	runtime.Identity = &resources.ServiceIdentity{
		Name:          "active-env",
		Module:        "mod",
		Workspace:     "test",
		WorkspacePath: runtime.Location,
	}
	runtime.Environment = shared.Must(resources.LocalEnvironment().Proto())

	source := filepath.Join(runtime.Location, "code")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "sample.py"),
		[]byte("def answer():\n    return 42\n"), 0o600))
	runtime.Service.SourceLocation = source

	return runtime, source
}

// TestActiveEnvironmentFollowsTheRunnerLifecycle pins the contract this agent
// owes the generic layer: what it builds is what Code, Tooling and the REPL
// resolve, and a teardown hands them nil rather than an environment whose
// docker client is already closed.
func TestActiveEnvironmentFollowsTheRunnerLifecycle(t *testing.T) {
	ctx := context.Background()
	runtime, _ := activeEnvRuntime(t)

	require.Nil(t, runtime.FastAPI.Service.ActiveEnv(),
		"nothing may be published before a runner environment exists")

	env, _, err := runtime.CreateRunnerEnvironment(ctx)
	require.NoError(t, err)
	require.Same(t, env, runtime.FastAPI.Service.ActiveEnv(),
		"Code must resolve the environment this agent actually runs in")

	_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Nil(t, runtime.FastAPI.Service.ActiveEnv(),
		"a shut-down environment must not stay reachable from Code")
}

// TestCodeIsSafeWhileTheRunnerLifecycleChurns runs the generic Code handler —
// the same instance main.go wires up — against a runtime publishing and
// clearing its environment underneath it. Code and the lifecycle RPCs are
// concurrent gRPC handlers, and the field they meet on lives in another
// module, so this is the only side that can drive both at once.
func TestCodeIsSafeWhileTheRunnerLifecycleChurns(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	ctx := context.Background()
	runtime, source := activeEnvRuntime(t)
	code := pythoncode.New(runtime.FastAPI.Service)

	done := make(chan struct{})
	var lifecycle sync.WaitGroup
	var lifecycleErr error
	lifecycle.Add(1)
	go func() {
		defer lifecycle.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_, _, err := runtime.CreateRunnerEnvironment(ctx)
			if err == nil {
				_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
			}
			if err != nil {
				lifecycleErr = err
				return
			}
		}
	}()

	for i := 0; i < 10; i++ {
		graph := code.ComputePythonCallGraph(ctx, source)
		require.Empty(t, graph.Error, "call graph %d must answer whatever the lifecycle is doing", i)
	}
	close(done)
	lifecycle.Wait()
	require.NoError(t, lifecycleErr, "the runner lifecycle must keep working under a concurrent reader")
}
