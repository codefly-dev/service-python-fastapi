package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
)

// destroyRuntime is a Runtime holding a real, running native process — the
// shape `codefly` shutdown reaches when it calls Destroy with no preceding
// Stop. The process touches a file in cacheLocation every 50ms, so a cache
// wiped while it was still alive gets repopulated and the ordering is
// observable rather than assumed.
func destroyRuntime(t *testing.T) (*Runtime, runners.Proc, string) {
	t.Helper()
	ctx := context.Background()

	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	runtime.Logger = runtime.Wool
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextNative()

	cache := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cache, "uv.json"), []byte("{}"), 0o600))
	runtime.cacheLocation = cache

	env, err := runners.NewNativeEnvironment(ctx, t.TempDir())
	require.NoError(t, err)
	proc, err := env.NewProcess("sh", "-c",
		"while true; do touch "+filepath.Join(cache, "heartbeat")+"; sleep 0.05; done")
	require.NoError(t, err)
	require.NoError(t, proc.Start(ctx))
	t.Cleanup(func() { _ = proc.Stop(context.Background()) })

	runtime.runner = &runnerHandle{proc: proc, cancel: func() {}}
	runtime.runnerEnvironment = env
	return runtime, proc, cache
}

func TestDestroyEndsExecutionBeforeClearingCache(t *testing.T) {
	ctx := context.Background()
	runtime, proc, cache := destroyRuntime(t)

	resp, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	alive, err := proc.IsRunning(ctx)
	require.NoError(t, err)
	require.False(t, alive, "Destroy must stop the service process without a preceding Stop")

	require.Nil(t, currentRunner(runtime))
	require.Nil(t, runtime.runnerEnvironment, "the runner environment must be shut down and released")

	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.Empty(t, entries, "the owned cache must be cleared")

	// Several heartbeat periods: a process that outlived the wipe repopulates.
	time.Sleep(500 * time.Millisecond)
	entries, err = os.ReadDir(cache)
	require.NoError(t, err)
	require.Empty(t, entries, "the cache must be cleared after execution ended, not before")
}

func TestDestroyStopsWatcher(t *testing.T) {
	ctx := context.Background()
	runtime, _, _ := destroyRuntime(t)
	require.NoError(t, os.MkdirAll(filepath.Join(runtime.Location, "code/src"), 0o700))

	require.NoError(t, runtime.SetupWatcher(ctx, services.NewWatchConfiguration(requirements), runtime.EventHandler))
	events := runtime.Events

	_, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)

	select {
	case _, open := <-events:
		require.False(t, open, "the watcher must be stopped, not still delivering events")
	case <-time.After(5 * time.Second):
		t.Fatal("watcher still running after Destroy")
	}
}

func TestDestroyReportsCacheFailureAfterEndingExecution(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the wipe cannot be made to fail")
	}
	ctx := context.Background()
	runtime, proc, cache := destroyRuntime(t)

	// Removing an entry needs write permission on its parent, so the wipe fails
	// while every other resource stays reachable.
	require.NoError(t, os.Chmod(cache, 0o500))
	t.Cleanup(func() { _ = os.Chmod(cache, 0o700) })

	resp, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_ERROR, resp.GetStatus().GetState(),
		"a failed cache wipe must be observable, not merely logged")
	require.Contains(t, resp.GetStatus().GetMessage(), "cannot remove cache")

	alive, err := proc.IsRunning(ctx)
	require.NoError(t, err)
	require.False(t, alive, "execution cleanup must complete even when the cache wipe fails")
	require.Nil(t, currentRunner(runtime))
	require.Nil(t, runtime.runnerEnvironment)
}

// stubbornProc fails every Stop, standing in for a process the agent cannot
// reap. Only Stop is ever reached on this path.
type stubbornProc struct{ runners.Proc }

func (stubbornProc) Stop(context.Context) error { return errors.New("process will not die") }

func TestDestroyReportsRunnerFailureAndStillClearsCache(t *testing.T) {
	ctx := context.Background()
	runtime, _, cache := destroyRuntime(t)
	runtime.runner = &runnerHandle{proc: stubbornProc{}, cancel: func() {}}

	resp, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_ERROR, resp.GetStatus().GetState(),
		"Destroy must not report success after only logging a failed process stop")
	require.Contains(t, resp.GetStatus().GetMessage(), "cannot stop the fastapi app")

	require.Nil(t, runtime.runnerEnvironment, "a failed stop must not strand the remaining cleanup")
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestDestroyIsIdempotent(t *testing.T) {
	ctx := context.Background()

	// Before Init: no runner, no environment, no cache location claimed.
	fresh := NewRuntime(NewService())
	fresh.Location = t.TempDir()
	resp, err := fresh.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	runtime, _, _ := destroyRuntime(t)
	for range 3 {
		resp, err = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
		require.NoError(t, err)
		require.Equal(t, runtimev0.DestroyStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	}

	// After Stop, Destroy still succeeds and has nothing left to release.
	stopped, _, _ := destroyRuntime(t)
	stop, err := stopped.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StopStatus_SUCCESS, stop.GetStatus().GetState(), stop.GetStatus().GetMessage())
	resp, err = stopped.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
}

// TestStartAfterDestroyCannotLeaveAReplacement covers the race `codefly`
// shutdown actually runs: a Start already in flight lands after Destroy has
// released the mutex. Destroy releases the execution environment, so that Start
// has nothing to launch into and fails loudly instead of leaving a uvicorn
// running against a wiped cache.
func TestStartAfterDestroyCannotLeaveAReplacement(t *testing.T) {
	ctx := context.Background()
	runtime, _, _ := destroyRuntime(t)

	_, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)

	start, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, start.GetStatus().GetState())
	require.Nil(t, currentRunner(runtime), "no process may survive Destroy")
}

// TestDestroyStopsRealUvicornWithoutStop is the acceptance case against the
// actual server: a listener that answers requests is gone before Destroy
// reports success, with no Stop anywhere in the path.
func TestDestroyStopsRealUvicornWithoutStop(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, servingApp)
	runtime := uvicornRuntime(t, source, false)
	runtime.cacheLocation = t.TempDir()

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serves(runtime.port) }, 30*time.Second, 200*time.Millisecond,
		"the fixture must be serving before Destroy means anything")

	handle := currentRunner(runtime)
	resp, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	running, err := handle.proc.IsRunning(ctx)
	require.NoError(t, err)
	require.False(t, running, "uvicorn must be gone before Destroy reports success")
	require.False(t, serves(runtime.port), "the listener must be gone before Destroy reports success")
}

// unstoppableEnv refuses to shut down, the state Destroy deliberately leaves
// reachable for a retry — and therefore the state in which a late Start still
// has somewhere to launch a process.
type unstoppableEnv struct {
	runners.RunnerEnvironment
	processes int
}

func (unstoppableEnv) Shutdown(context.Context) error {
	return errors.New("environment will not shut down")
}

func (e *unstoppableEnv) NewProcess(string, ...string) (runners.Proc, error) {
	e.processes++
	return &fakeProc{}, nil
}

func TestStartAfterFailedDestroyCannotLaunchReplacement(t *testing.T) {
	ctx := context.Background()
	runtime, _, _ := destroyRuntime(t)
	env := &unstoppableEnv{}
	runtime.runnerEnvironment = env

	resp, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_ERROR, resp.GetStatus().GetState())
	require.NotNil(t, runtime.runnerEnvironment,
		"a failed shutdown must keep the environment reachable for a retry")

	start, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, start.GetStatus().GetState())
	require.Zero(t, env.processes, "no replacement process may be launched after Destroy")
	require.Nil(t, currentRunner(runtime))
}

// TestInitReopensLifecycleAfterDestroy pins the other half of that contract: a
// deliberate Init is a new lifecycle, so Start works again rather than being
// refused forever by a runtime nobody can revive.
func TestInitReopensLifecycleAfterDestroy(t *testing.T) {
	ctx := context.Background()
	runtime, _, _ := destroyRuntime(t)

	_, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)

	runtime.runnerMu.Lock()
	require.True(t, runtime.destroyed)
	runtime.runnerMu.Unlock()

	// Init fails well before it would need a real environment; what matters is
	// that it reopened the lifecycle on its way through.
	_, _ = runtime.Init(ctx, &runtimev0.InitRequest{RuntimeContext: resources.NewRuntimeContextNative()})

	runtime.runnerMu.Lock()
	defer runtime.runnerMu.Unlock()
	require.False(t, runtime.destroyed, "Init must reopen the lifecycle it is starting")
}

func TestResolveRuntimeImageHonorsOverride(t *testing.T) {
	runtime := NewRuntime(NewService())

	image, err := runtime.resolveRuntimeImage()
	require.NoError(t, err)
	require.Equal(t, runtimeImage, image)

	runtime.FastAPI.Settings.RuntimeImage = "ghcr.io/acme/python:1.2.3"
	image, err = runtime.resolveRuntimeImage()
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/acme/python", image.Name)
	require.Equal(t, "1.2.3", image.Tag,
		"Destroy must reach for the image the service actually runs, not the default")

	runtime.FastAPI.Settings.RuntimeImage = "ghcr.io/acme/python:latest"
	_, err = runtime.resolveRuntimeImage()
	require.Error(t, err, "an unpinned override stays rejected")
}

// TestDestroyRemovesContainerBeforeClearingCache is the container half of the
// ordering contract. cacheLocation is the directory Init bind-mounts the venv
// and the uv cache out of, so wiping it while the container is up deletes the
// interpreter under the running service. runnerEnvironment is deliberately nil:
// that is the shape `codefly` shutdown reaches when Init never ran in this
// process, and the only path on which Destroy reaches the container by name.
func TestDestroyRemovesContainerBeforeClearingCache(t *testing.T) {
	ctx := context.Background()

	// Docker on macOS only bind-mounts shared paths, and the per-user temp dir
	// is not one — keep the mount inside the repo, as the lifecycle tests do.
	cache, err := os.MkdirTemp("testdata", "destroy-cache-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	cache = shared.MustSolvePath(cache)

	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	runtime.Logger = runtime.Wool
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextContainer()
	runtime.Identity = &resources.ServiceIdentity{
		Name:          fmt.Sprintf("destroy-order-%d", time.Now().UnixMilli()),
		Module:        "mod",
		Workspace:     "test",
		WorkspacePath: runtime.Location,
	}
	runtime.Environment = shared.Must(resources.LocalEnvironment().Proto())
	runtime.cacheLocation = cache

	env, err := dockerrun.NewDockerEnvironment(ctx, runtimeImage, t.TempDir(), runtime.Base.Runtime.UniqueWithWorkspace())
	if err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	env.WithPause()
	env.WithMount(cache, "/cache")
	require.NoError(t, env.Init(ctx))
	t.Cleanup(func() { _ = env.Shutdown(context.Background()) })

	proc, err := env.NewProcess("sh", "-c", "while true; do touch /cache/heartbeat; sleep 0.05; done")
	require.NoError(t, err)
	require.NoError(t, proc.Start(ctx))
	require.Eventually(t, func() bool {
		entries, readErr := os.ReadDir(cache)
		return readErr == nil && len(entries) > 0
	}, 30*time.Second, 100*time.Millisecond, "the container must be writing into the mounted cache")

	require.Nil(t, runtime.runnerEnvironment)
	resp, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.DestroyStatus_SUCCESS, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.Empty(t, entries, "the container must be gone before the cache it mounts is wiped")
}
