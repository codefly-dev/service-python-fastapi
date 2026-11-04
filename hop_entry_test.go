package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// TestMain pins the clock the admission deadline is read against (hopstack.go)
// for the whole suite. Without it every test that builds a source tree by hand
// — and so has no admission stack in it — would pass while the migration window
// is open and start failing on the day it closes, for a reason that has nothing
// to do with the change that happened to be in flight. The tests that assert
// the refusal set the clock themselves.
func TestMain(m *testing.M) {
	hopStackNow = func() time.Time { return hopStackRequiredFrom.Add(-24 * time.Hour) }
	os.Exit(m.Run())
}

// migrationClosed moves the clock past the deadline for one test.
func migrationClosed(t *testing.T) {
	t.Helper()
	previous := hopStackNow
	hopStackNow = func() time.Time { return hopStackRequiredFrom.Add(24 * time.Hour) }
	t.Cleanup(func() { hopStackNow = previous })
}

// unmigratedProject is a service as it was scaffolded before the hop's
// admission reached this agent: an app, and nothing that can verify a call.
func unmigratedProject(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(source, "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "pyproject.toml"), []byte(probeProject), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "main.py"), []byte("app = None\n"), 0o600))
	return source
}

// TestTheAdmissionIsTheAgentsAndNotTheServicesOwnSource is the ownership move:
// the wrap lives in a file this agent writes, so the one edit that used to
// remove it — deleting a line from src/main.py — is not an edit to anything
// that installs it any more.
func TestTheAdmissionIsTheAgentsAndNotTheServicesOwnSource(t *testing.T) {
	_, root := createServiceForTest(t, nil)
	code := filepath.Join(root, "code")

	entry, err := os.ReadFile(filepath.Join(code, hopEntryModule))
	require.NoError(t, err)
	require.Equal(t, hopEntrySource, string(entry), "the entry Create wrote is not the agent's")
	require.Contains(t, string(entry), "do not edit it")
	require.Contains(t, string(entry), "HopAdmission(application, ADMISSION)")

	// The one line that used to be the whole protection is gone from the file
	// an author owns: there is nothing there to delete.
	main, err := os.ReadFile(filepath.Join(code, "src", "main.py"))
	require.NoError(t, err)
	require.NotContains(t, string(main), "add_middleware(HopAdmission)")
	require.Contains(t, string(main), "codefly_hop_entry")

	stack, err := readHopStack(code)
	require.NoError(t, err)
	require.True(t, stack.complete(), "the scaffold does not carry the stack its entry imports")
	require.Equal(t, hopEntryApp, servedApp(stack))
}

// TestStartServesTheEntryAndRewritesIt is what makes the ownership hold over
// time: an author who deletes or edits the entry gets the agent's own file back
// on the next start, and what uvicorn is pointed at is never the application
// without the wrap.
func TestStartServesTheEntryAndRewritesIt(t *testing.T) {
	_, root := createServiceForTest(t, nil)
	code := filepath.Join(root, "code")
	runtime, env := startTestRuntime(t)
	runtime.Service.SourceLocation = code
	stack, err := readHopStack(code)
	require.NoError(t, err)
	runtime.hopStack = stack

	// An author's edit, of the only two kinds that matter: serving the
	// application unwrapped, and no entry at all.
	require.NoError(t, os.WriteFile(filepath.Join(code, hopEntryModule),
		[]byte("from src.main import app\n"), 0o644))

	_, err = runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, uvicornProcs(env), 1)
	require.Contains(t, uvicornProcs(env)[0].args, hopEntryApp)
	require.NotContains(t, uvicornProcs(env)[0].args, unadmittedApp)

	rewritten, err := os.ReadFile(filepath.Join(code, hopEntryModule))
	require.NoError(t, err)
	require.Equal(t, hopEntrySource, string(rewritten), "the start served an entry the agent did not write")
}

// TestTheImageServesTheSameEntryANativeStartServes: the recipe is the other
// place a listener is started, so the admission cannot be something only a
// native run has.
func TestTheImageServesTheSameEntryANativeStartServes(t *testing.T) {
	builder, _ := createdBuilder(t)
	outputDir := t.TempDir()
	dockerfile := dockerfileOf(t, outputDir, emitRecipe(t, builder, outputDir))
	require.Contains(t, dockerfile, fmt.Sprintf("code/%s ./", hopEntryModule))
	require.Contains(t, dockerfile, fmt.Sprintf(`CMD ["uvicorn", "%s"`, hopEntryApp))
	require.NotContains(t, dockerfile, fmt.Sprintf(`"%s"`, unadmittedApp))
}

// TestInitWarnsThenRefusesAServiceThatCannotAdmitACall is the installed base,
// on the path every run goes through. A service scaffolded before the admission
// serves every route to anything that can address its listener, and until the
// deadline the agent says so and starts it anyway — a refusal on the day this
// agent is released would stop services that worked yesterday from running at
// all. After the deadline the warning is a refusal that names the migration.
func TestInitWarnsThenRefusesAServiceThatCannotAdmitACall(t *testing.T) {
	isolateWorkingDir(t)
	runtime, req := initTestRuntime(t)
	runtime.Service.SourceLocation = unmigratedProject(t)

	open, err := runtime.Init(context.Background(), req)
	require.NoError(t, err)
	require.NotEqual(t, runtimev0.InitStatus_ERROR, open.GetStatus().GetState(), open.GetStatus().GetMessage())
	require.Equal(t, unadmittedApp, servedApp(runtime.hopStack),
		"a service with no verifier was pointed at an entry whose import cannot resolve")

	migrationClosed(t)
	closed, err := runtime.Init(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_ERROR, closed.GetStatus().GetState())
	require.Contains(t, closed.GetStatus().GetMessage(), "src/hop.py does not define class HopAdmission")
	require.Contains(t, closed.GetStatus().GetMessage(), "docs/hop-migration.md")
}

// TestDeployRefusesAServiceThatCannotAdmitACall is the same refusal where it
// matters most: a deployment puts the listener where anything that can address
// the cluster reaches it.
func TestDeployRefusesAServiceThatCannotAdmitACall(t *testing.T) {
	builder, _ := createdBuilder(t)
	builder.SourceLocation = unmigratedProject(t)
	migrationClosed(t)

	_, err := builder.Deploy(context.Background(), &builderv0.DeploymentRequest{})
	require.ErrorContains(t, err, "no listener it starts verifies the hop's admission")
	require.ErrorContains(t, err, "docs/hop-migration.md")
}

// TestUpdateMovesALegacyReadinessProbeOffTheAdmittedRoute is the trap a hand
// migration falls into: the probe the manifest was scaffolded with points at
// /version, the admission admits /version like every other route, a kubelet
// carries no admission, and so every replica reads 401 and is NotReady for
// ever with nothing in the rollout naming why.
func TestUpdateMovesALegacyReadinessProbeOffTheAdmittedRoute(t *testing.T) {
	builder, _ := createdBuilder(t)
	ctx := context.Background()

	legacy := builder.Base.Service.Endpoints[0]
	legacy.Health = &resources.Health{
		Readiness: &resources.Probe{Kind: resources.ProbeKindHTTP, Path: legacyReadinessPath, Statuses: []string{"200"}},
		Startup:   &resources.Probe{Kind: resources.ProbeKindTransport, Period: "2s", FailureThreshold: 30},
		Liveness:  &resources.Probe{Kind: resources.ProbeKindTransport},
	}
	require.NoError(t, builder.Base.Service.Save(ctx))

	_, err := builder.Update(ctx, &builderv0.UpdateRequest{})
	require.NoError(t, err)
	require.Equal(t, hopReadinessPath, builder.Base.Service.Endpoints[0].Health.Readiness.Path)

	// Recorded, not only held in memory: the probe the kubelet gets is the one
	// on disk, read back by whatever loads the service next.
	reloaded, err := resources.LoadServiceFromDir(ctx, builder.Location)
	require.NoError(t, err)
	require.Equal(t, hopReadinessPath, reloaded.Endpoints[0].Health.Readiness.Path)
}

// TestUpdateLeavesAnUnmigratedReadinessProbeAlone: a service with no admission
// still serves /version to a kubelet, and pointing its probe at a route it does
// not have would take out exactly the replicas this is here to keep up.
func TestUpdateLeavesAnUnmigratedReadinessProbeAlone(t *testing.T) {
	builder, _ := createdBuilder(t)
	ctx := context.Background()
	builder.SourceLocation = unmigratedProject(t)

	legacy := builder.Base.Service.Endpoints[0]
	legacy.Health = &resources.Health{
		Readiness: &resources.Probe{Kind: resources.ProbeKindHTTP, Path: legacyReadinessPath, Statuses: []string{"200"}},
		Startup:   &resources.Probe{Kind: resources.ProbeKindTransport, Period: "2s", FailureThreshold: 30},
		Liveness:  &resources.Probe{Kind: resources.ProbeKindTransport},
	}
	require.NoError(t, builder.Base.Service.Save(ctx))

	_, err := builder.Update(ctx, &builderv0.UpdateRequest{})
	require.NoError(t, err)
	require.Equal(t, legacyReadinessPath, builder.Base.Service.Endpoints[0].Health.Readiness.Path)
}

// TestTheEntryAdmitsARouteTheApplicationNeverAsksToBeAdmitted is the property,
// over a real socket: the application the author owns installs nothing, and a
// route of theirs is still refused unstamped and served stamped.
func TestTheEntryAdmitsARouteTheApplicationNeverAsksToBeAdmitted(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a scaffolded service with uv")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not installed: the scaffolded service cannot run")
	}
	_, root := createServiceForTest(t, nil)
	code := filepath.Join(root, "code")
	// A route an author adds, in a file of their own, with no admission of any
	// kind written anywhere near it.
	require.NoError(t, os.WriteFile(filepath.Join(code, "src", "plugins", "registry.py"),
		[]byte(authoredPlugin), 0o644))

	authority := serveHopAuthority(t)
	app := bootScaffold(t, code, false, authority)
	t.Cleanup(func() { app.stop(t) })
	address := fmt.Sprintf("http://127.0.0.1:%d", app.rest)
	client := http.Client{Timeout: 5 * time.Second}

	refused, err := client.Get(address + "/authored")
	require.NoError(t, err)
	_ = refused.Body.Close()
	require.Equal(t, http.StatusUnauthorized, refused.StatusCode,
		"a route the author added was served without the hop's admission")

	admitted, err := authority.get(t, &client, address, "/authored")
	require.NoError(t, err)
	_ = admitted.Body.Close()
	require.Equal(t, http.StatusOK, admitted.StatusCode)
}

// authoredPlugin is a plugin as a service author writes one: a router, and
// nothing about authentication.
const authoredPlugin = `from fastapi import APIRouter

from src.framework.plugin import Plugin

router = APIRouter()


@router.get("/authored")
async def authored():
    return {"authored": True}


plugins = [Plugin(name="authored", router=router)]
`

// TestTheMigrationDeadlineIsStatedWhereAServiceOwnerMeetsIt keeps the date in
// the code and the date in the procedure one date: a document naming a deadline
// the agent does not enforce, or the other way round, is worse than neither.
func TestTheMigrationDeadlineIsStatedWhereAServiceOwnerMeetsIt(t *testing.T) {
	procedure, err := os.ReadFile(filepath.Join("docs", "hop-migration.md"))
	require.NoError(t, err)
	stated := hopStackRequiredFrom.Format(time.DateOnly)
	require.Contains(t, string(procedure), stated,
		"docs/hop-migration.md does not state the date hopStackRequiredFrom enforces")
	require.Contains(t, hopStackWarning(hopStack{}), stated)
	require.Contains(t, hopStackRefusal(hopStack{}).Error(), stated)
	require.True(t, strings.Contains(string(procedure), hopEntryModule),
		"the procedure does not name the entry a migrated service is served through")
}
