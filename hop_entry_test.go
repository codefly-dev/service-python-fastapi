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
	require.Contains(t, string(entry), "_WRAP = HopAdmission(")

	// The order is the property: the open set, the preflight test and the
	// wrapper function are read out of src/hop.py BEFORE src.main is imported,
	// so a line in src/main.py that rebinds them reaches nothing a request is
	// tested against.
	source := string(entry)
	require.Less(t, strings.Index(source, "_OPEN_PATHS = tuple(OPEN_PATHS)"),
		strings.Index(source, "from src.main import"),
		"the entry reads the open set after importing the application")
	require.Less(t, strings.Index(source, "_ADMIT = HopAdmission.__call__"),
		strings.Index(source, "from src.main import"),
		"the entry takes hold of the wrapper after importing the application")
	require.Less(t, strings.Index(source, "_WRAP = HopAdmission("),
		strings.Index(source, "from src.main import"),
		"the entry builds the wrap after importing the application")

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

// migratedSource is a hand migration as far as the gate can see it: the module
// defining the class, and the dependency. serves decides whether the open
// readiness route is there too, which is the half an owner forgets.
func migratedSource(t *testing.T, serves bool) string {
	t.Helper()
	source := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(source, "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "pyproject.toml"),
		[]byte("[project]\nname = \"m\"\nversion = \"0.0.0\"\ndependencies = [\"authority-client\"]\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "hop.py"),
		[]byte("class HopAdmission:\n    pass\n"), 0o600))
	main := "app = None\n"
	if serves {
		main += "# @router.get(\"/ready\")\nREADY = \"/ready\"\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "main.py"), []byte(main), 0o600))
	return source
}

// legacyReadiness puts the probe every pre-admission scaffold declared on the
// builder's loaded service and records it.
func legacyReadiness(t *testing.T, builder *Builder, path string, statuses []string) {
	t.Helper()
	builder.Base.Service.Endpoints[0].Health = &resources.Health{
		Readiness: &resources.Probe{Kind: resources.ProbeKindHTTP, Path: path, Statuses: statuses},
		Startup:   &resources.Probe{Kind: resources.ProbeKindTransport, Period: "2s", FailureThreshold: 30},
		Liveness:  &resources.Probe{Kind: resources.ProbeKindTransport},
	}
	require.NoError(t, builder.Base.Service.Save(context.Background()))
}

// TestAServiceThatInstallsTheWrapItselfIsRefused: the entry is the only wrap.
// An admission is single-use, so a second one verifies it twice and refuses
// every call as stamp_replayed. The earlier answer — letting an already-marked
// scope through — is what let an application mark every scope from its own
// lifespan and serve unadmitted, so this is refused rather than absorbed, and
// refused on sight rather than on a date.
func TestAServiceThatInstallsTheWrapItselfIsRefused(t *testing.T) {
	isolateWorkingDir(t)
	source := migratedSource(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "main.py"),
		[]byte("app = None\napp.add_middleware(HopAdmission)\n"), 0o600))

	stack, err := readHopStack(source)
	require.NoError(t, err)
	require.True(t, stack.secondWrap)
	refusal, warn := checkHopStack(stack)
	require.Error(t, refusal)
	require.Empty(t, warn)
	require.ErrorContains(t, refusal, "stamp_replayed")

	runtime, req := initTestRuntime(t)
	runtime.Service.SourceLocation = source
	resp, err := runtime.Init(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, runtimev0.InitStatus_ERROR, resp.GetStatus().GetState())
	require.Contains(t, resp.GetStatus().GetMessage(), "the entry is the only wrap")
}

// TestStartRefusesAfterTheDeadlineEvenWhenInitPassed: Init answers a response
// field, not a Go error, so a caller that branches on the error reaches Start —
// and a long-lived agent can reach a Start after the date has turned. Start is
// the one call that opens the socket, so Start is where the refusal has to be.
func TestStartRefusesAfterTheDeadlineEvenWhenInitPassed(t *testing.T) {
	isolateWorkingDir(t)
	runtime, req := initTestRuntime(t)
	runtime.Service.SourceLocation = unmigratedProject(t)
	runtime.runnerEnvironment = &fakeRunnerEnvironment{}

	passed, err := runtime.Init(context.Background(), req)
	require.NoError(t, err)
	require.NotEqual(t, runtimev0.InitStatus_ERROR, passed.GetStatus().GetState())

	migrationClosed(t)
	started, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, started.GetStatus().GetState())
	require.Contains(t, started.GetStatus().GetMessage(), "docs/hop-migration.md")
}

// TestStartRewritesTheEntryEvenWhenItKeepsTheProcess is the hot-reload hole:
// uvicorn re-imports whatever is on disk inside the running process, and the
// branch that keeps a process used to return before the rewrite.
func TestStartRewritesTheEntryEvenWhenItKeepsTheProcess(t *testing.T) {
	_, root := createServiceForTest(t, nil)
	code := filepath.Join(root, "code")
	runtime, env := startTestRuntime(t)
	runtime.Service.SourceLocation = code

	ctx := context.Background()
	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, uvicornProcs(env), 1)

	// An edit during the developer run, then a Start with identical inputs:
	// the process is kept, and the entry is still the agent's.
	require.NoError(t, os.WriteFile(filepath.Join(code, hopEntryModule),
		[]byte("from src.main import app\n"), 0o644))
	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Len(t, uvicornProcs(env), 1, "the process was replaced, so this is not the branch under test")

	kept, err := os.ReadFile(filepath.Join(code, hopEntryModule))
	require.NoError(t, err)
	require.Equal(t, hopEntrySource, string(kept))
}

// TestAPackageCannotShadowTheEntry: CPython resolves a package before a
// same-named module in one sys.path entry, and uvicorn puts the working
// directory first — so code/codefly_hop_entry/ would win over the agent's file
// and serve the unwrapped application while the agent rewrote a module nothing
// imports.
func TestAPackageCannotShadowTheEntry(t *testing.T) {
	_, root := createServiceForTest(t, nil)
	code := filepath.Join(root, "code")
	shadow := filepath.Join(code, hopEntryPackage)
	require.NoError(t, os.MkdirAll(shadow, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(shadow, "__init__.py"),
		[]byte("from src.main import app\n"), 0o644))

	require.NoError(t, writeHopEntry(code))
	_, err := os.Stat(shadow)
	require.True(t, os.IsNotExist(err), "the shadowing package survived the entry being written")
}

// TestSyncMovesTheReadinessProbeWithTheEntry pairs the command the migration
// document names with the step it promises. Writing the entry is what makes
// /version admitted, so writing it without moving the probe is the NotReady
// failure the document exists to prevent.
func TestSyncMovesTheReadinessProbeWithTheEntry(t *testing.T) {
	builder, _ := createdBuilder(t)
	ctx := context.Background()
	legacyReadiness(t, builder, legacyReadinessPath, []string{"200"})

	_, err := builder.Sync(ctx, &builderv0.SyncRequest{})
	require.NoError(t, err)
	require.Equal(t, hopReadinessPath, builder.Base.Service.Endpoints[0].Health.Readiness.Path)

	entry, err := os.ReadFile(filepath.Join(builder.SourceLocation, hopEntryModule))
	require.NoError(t, err)
	require.Equal(t, hopEntrySource, string(entry))
}

// TestAHalfwayMigrationKeepsItsReadinessProbe: the stack is there, the open
// route is not. Moving the probe would point a kubelet at a 404 and take out
// every replica — the failure the move exists to prevent.
func TestAHalfwayMigrationKeepsItsReadinessProbe(t *testing.T) {
	builder, _ := createdBuilder(t)
	ctx := context.Background()
	builder.SourceLocation = migratedSource(t, false)
	legacyReadiness(t, builder, legacyReadinessPath, []string{"200"})

	_, err := builder.Update(ctx, &builderv0.UpdateRequest{})
	require.NoError(t, err)
	require.Equal(t, legacyReadinessPath, builder.Base.Service.Endpoints[0].Health.Readiness.Path)
}

// TestTheReadinessMoveReverses: an owner who starts a migration, hits a problem
// and takes the stack back out was left pointing at /ready on a service that no
// longer serves it — permanently NotReady, with nothing willing to touch it.
func TestTheReadinessMoveReverses(t *testing.T) {
	builder, _ := createdBuilder(t)
	ctx := context.Background()
	require.NoError(t, os.Remove(filepath.Join(builder.SourceLocation, "src", "hop.py")))
	legacyReadiness(t, builder, hopReadinessPath, []string{"200"})

	_, err := builder.Update(ctx, &builderv0.UpdateRequest{})
	require.NoError(t, err)
	require.Equal(t, legacyReadinessPath, builder.Base.Service.Endpoints[0].Health.Readiness.Path)
}

// TestTheReadinessMoveKeepsAuthoredStatuses: the accepted statuses are the
// author's declaration, not this agent's to replace while moving a path.
func TestTheReadinessMoveKeepsAuthoredStatuses(t *testing.T) {
	builder, _ := createdBuilder(t)
	ctx := context.Background()
	legacyReadiness(t, builder, legacyReadinessPath, []string{"200", "204"})

	_, err := builder.Update(ctx, &builderv0.UpdateRequest{})
	require.NoError(t, err)
	readiness := builder.Base.Service.Endpoints[0].Health.Readiness
	require.Equal(t, hopReadinessPath, readiness.Path)
	require.Equal(t, []string{"200", "204"}, readiness.Statuses)
}

// TestBuildRefusesAServiceThatCannotAdmitACall is the one place an unadmitted
// IMAGE is produced, and it had no test: TestMain pins the clock inside the
// migration window, so the refusal branch was never reached.
func TestBuildRefusesAServiceThatCannotAdmitACall(t *testing.T) {
	builder, _ := createdBuilder(t)
	builder.SourceLocation = unmigratedProject(t)
	migrationClosed(t)

	resp, err := builder.Build(t.Context(), recipeBuildRequest(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
	require.Contains(t, resp.GetState().GetMessage(), "docs/hop-migration.md")
}

// TestAPackagedVerifierIsNotMistakenForAnAbsentOne: src/hop/__init__.py is an
// ordinary refactor as the module grows, and reading only src/hop.py made it
// an EISDIR that failed Init, Build and Deploy with "is a directory".
func TestAPackagedVerifierIsNotMistakenForAnAbsentOne(t *testing.T) {
	source := migratedSource(t, true)
	require.NoError(t, os.Remove(filepath.Join(source, "src", "hop.py")))
	require.NoError(t, os.MkdirAll(filepath.Join(source, "src", "hop"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(source, "src", "hop", "__init__.py"),
		[]byte("class HopAdmission:\n    pass\n"), 0o644))

	stack, err := readHopStack(source)
	require.NoError(t, err)
	require.True(t, stack.complete(), "a packaged verifier was read as absent")
}
