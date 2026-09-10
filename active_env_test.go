package main

import (
	"context"
	"os/exec"
	"sync"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	"github.com/stretchr/testify/require"
)

// TestActiveEnvironmentFollowsTheRunnerLifecycle pins the contract this agent
// owes the generic layer: what it builds is what Code, Tooling and the REPL
// resolve, and a teardown hands them nil rather than an environment whose
// docker client is already closed.
func TestActiveEnvironmentFollowsTheRunnerLifecycle(t *testing.T) {
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, _ := initTestRuntime(t)

	require.Nil(t, runtime.FastAPI.Service.ActiveEnvironment(),
		"nothing may be published before a runner environment exists")

	env, _, err := runtime.CreateRunnerEnvironment(ctx)
	require.NoError(t, err)
	require.Same(t, env, runtime.FastAPI.Service.ActiveEnvironment(),
		"Code must resolve the environment this agent actually runs in")

	_, err = runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Nil(t, runtime.FastAPI.Service.ActiveEnvironment(),
		"a shut-down environment must not stay reachable from Code")
}

// TestCodeKeepsAnsweringWhileTheRunnerLifecycleChurns runs the generic Code
// handler — the same instance main.go wires up — against a runtime publishing
// and clearing its environment underneath it. Code and the lifecycle RPCs are
// concurrent gRPC handlers, and the field they meet on lives in another
// module, so this is the only side that can drive both at once.
//
// Named for what it actually pins: no data race and no failed request,
// whichever side wins. It deliberately does not assert that a read observed
// the published environment — that is timing-dependent and would flake on a
// loaded runner. TestActiveEnvironmentFollowsTheRunnerLifecycle pins the
// publish deterministically instead.
func TestCodeKeepsAnsweringWhileTheRunnerLifecycleChurns(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	isolateWorkingDir(t)
	ctx := context.Background()
	runtime, _ := initTestRuntime(t)
	source := runtime.Service.SourceLocation
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
