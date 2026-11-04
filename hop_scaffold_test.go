package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/stretchr/testify/require"
)

// TestTheScaffoldProvesItsOwnAdmission runs a generated service's own pytest
// suite, which is where the rule is proved against the real authority client:
// an unstamped call is refused, a call the hop stamped is served, an admission
// is single-use and bound to its own route and module, health is open, and the
// service refuses to start without the verifier's inputs
// (templates/factory/code/tests/hop/test_admission.py.tmpl, and the gRPC half
// in templates/grpc/code/tests/rpc/test_grpc.py.tmpl).
//
// The suite ships to every service this agent creates, so this test is also the
// guard on the fixture: a template change that leaves the generated tests unable
// to stamp a call is a change every user's suite would break on.
func TestTheScaffoldProvesItsOwnAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a service and resolves its dependencies with uv")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: the scaffold's own suite cannot run")
	}
	_, root := createServiceForTest(t, map[string]*agentv0.Answer{
		HotReload:      confirmAnswer(false),
		PublicEndpoint: confirmAnswer(false),
		GRPCServer:     confirmAnswer(true),
	})
	code := filepath.Join(root, "code")
	// The gRPC stubs `codefly sync` would have written; the scaffold's own
	// conftest generates them too, but not before this process resolves.
	generateScaffoldStubs(t, code)

	suite := exec.Command("uv", "run", "pytest", "-q", "-p", "no:cacheprovider")
	suite.Dir = code
	// CODEFLY__ENVIRONMENT is exported, and not as "local": a pipeline that
	// names its run is the ordinary case, and the suite's own fixtures have to
	// hold whatever the run is called. Reserving admissions in the process is
	// the test process's decision, which conftest.py makes for itself.
	suite.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "CODEFLY__ENVIRONMENT=ci")
	out, err := suite.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

// TestAGeneratedServiceRefusesToStartWithoutTheVerifiersInputs is the other half
// of the rule, end to end: a listener that cannot verify an admission must not
// serve. With no authority/endpoint delivered, uvicorn's startup fails, the
// process exits naming what is missing, and the port is never bound — rather
// than the app coming up and serving a route on keys it never read.
func TestAGeneratedServiceRefusesToStartWithoutTheVerifiersInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a service and resolves its dependencies with uv")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: the scaffolded service cannot run")
	}
	_, root := createServiceForTest(t, nil)
	port := freePort(t)
	require.NoError(t, writeHopEntry(filepath.Join(root, "code")))
	app := exec.Command("uv", "run", "uvicorn", hopEntryApp,
		"--host", "127.0.0.1", "--port", fmt.Sprint(port))
	app.Dir = filepath.Join(root, "code")
	var environment []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "CODEFLY__WORKSPACE_CONFIGURATION__AUTHORITY__") {
			environment = append(environment, variable)
		}
	}
	// Everything but where the authority answers.
	app.Env = append(environment, "CODEFLY__ENVIRONMENT=local", "CODEFLY__MODULE="+hopModule)
	output := &lockedOutput{}
	app.Stdout, app.Stderr = output, output
	require.NoError(t, app.Start())
	exited := make(chan error, 1)
	go func() { exited <- app.Wait() }()
	t.Cleanup(func() { _ = app.Process.Kill() })

	select {
	case <-exited:
	case <-time.After(3 * time.Minute): // a cold uv sync
		t.Fatalf("the app served without the verifier's inputs:\n%s", output)
	}
	require.Contains(t, output.String(), "authority/endpoint is required")
	require.False(t, listens(port), "the app bound its port without a verifier")
}

// TestTheGeneratedAppAdmitsOnlyStampedRoutesOverItsOwnListener is the REST half
// over a real socket: the scaffold's own uvicorn, reached the way a caller
// reaches it rather than through an in-process ASGI transport.
//
// It is here rather than only in testCreateToRun because TestCreateToRunNative
// is guarded by a poetry runtime this repo does not even support, so it skips
// silently on most hosts — and a skip is not a pass.
//
// bootScaffold waiting at all is part of the proof: it waits for /ready, which
// answers 200 only once the replica has read this authority's published keys.
func TestTheGeneratedAppAdmitsOnlyStampedRoutesOverItsOwnListener(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a scaffolded service with uv")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: the scaffolded service cannot run")
	}
	_, root := createServiceForTest(t, nil)
	authority := serveHopAuthority(t)
	app := bootScaffold(t, filepath.Join(root, "code"), false, authority)
	t.Cleanup(func() { app.stop(t) })

	address := fmt.Sprintf("http://127.0.0.1:%d", app.rest)
	client := http.Client{Timeout: 5 * time.Second}

	// Open, because a probe is not a caller.
	for _, open := range []string{"/health", "/ready"} {
		probe, err := client.Get(address + open)
		require.NoError(t, err)
		_ = probe.Body.Close()
		require.Equal(t, http.StatusOK, probe.StatusCode, "%s is not served unauthenticated", open)
	}

	refused, err := client.Get(address + "/version")
	require.NoError(t, err)
	_ = refused.Body.Close()
	require.Equal(t, http.StatusUnauthorized, refused.StatusCode,
		"the generated app served a route the hop did not stamp")

	admitted, err := authority.get(t, &client, address, "/version")
	require.NoError(t, err)
	body, err := io.ReadAll(admitted.Body)
	_ = admitted.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, admitted.StatusCode, "%s", body)
	require.Contains(t, string(body), "version")
}
