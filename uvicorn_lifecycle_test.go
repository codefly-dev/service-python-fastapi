package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
)

// These tests drive the real thing: `uv run uvicorn` against a throwaway
// project, because the defects they cover — a process that dies after Start
// committed STARTED, a second Start taking ownership of a port that already has
// a listener — only exist in the interaction with an actual process. The
// fixture is a bare ASGI callable rather than a FastAPI app: uvicorn is the
// process under test, and pulling FastAPI in would only make the sync slower.

const (
	// servingApp answers every request, which is what "ready" means here.
	servingApp = `async def app(scope, receive, send):
    await send({"type": "http.response.start", "status": 200, "headers": [[b"content-type", b"text/plain"]]})
    await send({"type": "http.response.body", "body": b"ok"})
`
	// brokenApp fails at import, the way a service does when a dependency is
	// missing or a module-level statement raises.
	brokenApp = `raise ImportError("codefly-lifecycle-boom")
`
	uvicornProject = `[project]
name = "lifecycle-probe"
version = "0.0.0"
requires-python = ">=3.10"
dependencies = ["uvicorn>=0.30"]
`
)

// uvProject materializes a synced uv project holding app as src/main.py. It
// skips rather than fails when uv or its network are missing: the prerequisite
// is named so a skipped run is not read as a passing one.
func uvProject(t *testing.T, app string) string {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: the real uvicorn lifecycle cannot be exercised")
	}
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(uvicornProject), 0o644))
	writeApp(t, dir, app)

	sync := exec.Command("uv", "sync")
	sync.Dir = dir
	if out, err := sync.CombinedOutput(); err != nil {
		t.Skipf("uv sync failed, uvicorn is unavailable: %v\n%s", err, out)
	}
	return dir
}

func writeApp(t *testing.T, dir, app string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "main.py"), []byte(app), 0o644))
}

// uvicornRuntime stands a Runtime up at the point Init leaves it: a native
// runner environment over the project, and a port to bind.
func uvicornRuntime(t *testing.T, source string, hotReload bool) *Runtime {
	t.Helper()
	ctx := context.Background()

	runtime := NewRuntime(NewService())
	runtime.Location = t.TempDir()
	runtime.Logger = runtime.Wool
	runtime.Service.SourceLocation = source
	runtime.FastAPI.Settings.HotReload = hotReload

	env, err := runners.NewNativeEnvironment(ctx, source)
	require.NoError(t, err)
	runtime.runnerEnvironment = env
	runtime.port = freePort(t)

	t.Cleanup(func() {
		_, _ = runtime.Stop(context.Background(), &runtimev0.StopRequest{})
	})
	return runtime
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return uint16(port)
}

// serves reports whether the app answers on its own address — the readiness
// question, as opposed to "is a process alive".
func serves(port uint16) bool {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func runtimeStartState(t *testing.T, runtime *Runtime) *runtimev0.StartStatus {
	t.Helper()
	info, err := runtime.Base.Runtime.InformationResponse(context.Background(), &runtimev0.InformationRequest{})
	require.NoError(t, err)
	return info.GetStartStatus()
}

// TestUvicornDeathRevokesStartedAndSourceFixRecovers is the headline case: an
// import error kills uvicorn seconds after Start returned STARTED. The status
// has to follow the process down, carry the traceback that explains it, and a
// later Start against fixed source has to bring the service back.
func TestUvicornDeathRevokesStartedAndSourceFixRecovers(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, brokenApp)
	runtime := uvicornRuntime(t, source, false)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	require.Eventually(t, func() bool {
		return runtimeStartState(t, runtime).GetState() == runtimev0.StartStatus_ERROR
	}, 30*time.Second, 100*time.Millisecond, "a uvicorn that died on an import error must not stay STARTED")
	require.Contains(t, runtimeStartState(t, runtime).GetMessage(), "codefly-lifecycle-boom",
		"the diagnostic must name what killed the process")

	writeApp(t, source, servingApp)
	resp, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	require.Eventually(t, func() bool { return serves(runtime.port) }, 30*time.Second, 200*time.Millisecond,
		"fixing the source must let a later start serve")
	require.Equal(t, runtimev0.StartStatus_STARTED, runtimeStartState(t, runtime).GetState())
}

// TestRepeatedStartWithoutHotReloadOwnsOneProcess pins process ownership for
// the setting that used to skip the reuse check: an unchanged request must keep
// the process that is serving, and a changed one must reap it before binding
// the port again.
func TestRepeatedStartWithoutHotReloadOwnsOneProcess(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, servingApp)
	runtime := uvicornRuntime(t, source, false)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serves(runtime.port) }, 30*time.Second, 200*time.Millisecond)

	first := currentRunner(runtime)

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Same(t, first, currentRunner(runtime), "an unchanged request must not launch a second uvicorn")
	require.True(t, serves(runtime.port))

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{Fixture: "seed"})
	require.NoError(t, err)
	require.NotSame(t, first, currentRunner(runtime), "changed inputs need a process carrying them")

	running, err := first.proc.IsRunning(ctx)
	require.NoError(t, err)
	require.False(t, running, "the replaced process must be gone, not left on the port")

	require.Eventually(t, func() bool { return serves(runtime.port) }, 30*time.Second, 200*time.Millisecond,
		"the replacement must own the port the old process released")
}

// TestHotReloadParentOutlivesBrokenApp is the boundary this agent must not blur.
// uvicorn --reload keeps its supervisor alive when the application fails to
// import, so process liveness stays STARTED while nothing serves: readiness is
// the orchestrator's endpoint probe, not this status. A reload of fixed source
// then restores the serving app without a new process.
func TestHotReloadParentOutlivesBrokenApp(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, brokenApp)
	runtime := uvicornRuntime(t, source, true)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	require.Never(t, func() bool {
		return runtimeStartState(t, runtime).GetState() == runtimev0.StartStatus_ERROR
	}, 5*time.Second, 250*time.Millisecond, "the reload supervisor is alive, so the process did not exit")
	require.False(t, serves(runtime.port), "an app that failed to import serves nothing, whatever the process status says")

	supervisor := currentRunner(runtime)
	writeApp(t, source, servingApp)
	require.Eventually(t, func() bool { return serves(runtime.port) }, 30*time.Second, 200*time.Millisecond,
		"a successful reload must restore a serving app")
	require.Same(t, supervisor, currentRunner(runtime), "the reload happens inside the process uvicorn already owns")
}
