package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// What a native start reports it ran (runtime-started-artifact/v1). The headline
// cases drive a real `uv run uvicorn`, because the question these answer is
// whether the agent names the interpreter and the files a live process is
// actually at — which an assertion against a stub would not test. The fixture
// app reports its own `sys.executable` and `__file__` over HTTP, so the process
// under test is the witness rather than this test's own idea of a venv layout.

// reportingApp answers with the interpreter running it and the file it was
// imported from.
const reportingApp = `import sys


async def app(scope, receive, send):
    body = ("executable=" + sys.executable + "\nfile=" + __file__).encode()
    await send({"type": "http.response.start", "status": 200, "headers": [[b"content-type", b"text/plain"]]})
    await send({"type": "http.response.body", "body": body})
`

// ranAs is what the serving process says it is running as.
type ranAs struct {
	executable string
	file       string
}

// servedIdentity asks the running app what it is. It is the ground truth these
// tests compare the agent's report against.
func servedIdentity(t *testing.T, port uint16) ranAs {
	t.Helper()
	var identity ranAs
	require.Eventually(t, func() bool {
		client := http.Client{Timeout: time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false
		}
		for line := range strings.SplitSeq(string(body), "\n") {
			key, value, found := strings.Cut(line, "=")
			switch {
			case !found:
			case key == "executable":
				identity.executable = value
			case key == "file":
				identity.file = value
			}
		}
		return identity.executable != "" && identity.file != ""
	}, 30*time.Second, 200*time.Millisecond, "the fixture app never reported what it runs as")
	return identity
}

// requireDeliverable holds the artifact to the two rules the consumer refuses
// on: every path is absolute, and every path is a regular file it can digest
// (cli#993). A report that breaks either fails the native delivery of a service
// that is up, so it is asserted here rather than discovered there.
func requireDeliverable(t *testing.T, artifact *runtimev0.StartedArtifact) {
	t.Helper()
	require.NotEmpty(t, artifact.GetExecutable(), "the artifact names no program")
	for _, path := range append([]string{artifact.GetExecutable()}, artifact.GetSources()...) {
		require.True(t, filepath.IsAbs(path), "%s is not an absolute path", path)
		info, err := os.Stat(path)
		require.NoErrorf(t, err, "the agent reported %s, which cannot be read", path)
		require.Truef(t, info.Mode().IsRegular(), "the agent reported %s, which is not a regular file", path)
	}
}

// requireNamesTheSameFile finds want among sources by file identity rather than
// by spelling. A macOS temporary directory is reached as /var and reported by
// python as /private/var: the agent names each file the way its own location
// was handed to it, and the consumer canonicalizes every path's directory
// before digesting it (cli#989), so two spellings of one file are one build.
// Comparing strings here would assert a canonicalization the contract puts on
// the consumer.
func requireNamesTheSameFile(t *testing.T, sources []string, want, message string) {
	t.Helper()
	wanted, err := os.Stat(want)
	require.NoError(t, err)
	for _, source := range sources {
		if info, err := os.Stat(source); err == nil && os.SameFile(wanted, info) {
			return
		}
	}
	require.Failf(t, message, "%s is not among %v", want, sources)
}

// TestNativeStartNamesTheInterpreterAndSourcesThatActuallyRan is the headline
// case: the program the agent reports has to be the interpreter the serving
// process is running, and the sources have to include the entrypoint that
// process imported. Both are read from the process itself.
func TestNativeStartNamesTheInterpreterAndSourcesThatActuallyRan(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, reportingApp)
	runtime := uvicornRuntime(t, source, false)
	runtime.serveAddress = fmt.Sprintf("127.0.0.1:%d", runtime.port)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())

	artifact := resp.GetStatus().GetStartedArtifact()
	require.NotNil(t, artifact, "a native start must name the build it ran, or a native run refuses the service")

	ran := servedIdentity(t, runtime.port)
	requireNamesTheSameFile(t, []string{artifact.GetExecutable()}, ran.executable,
		"the reported program is not the interpreter the serving process is running")
	requireNamesTheSameFile(t, artifact.GetSources(), ran.file,
		"the reported sources leave out the entrypoint the serving process imported")
	requireDeliverable(t, artifact)

	// The lock is what pins everything the interpreter imports beyond the
	// service's own tree, so an artifact without it describes a build that
	// could be any set of dependencies.
	requireNamesTheSameFile(t, artifact.GetSources(), filepath.Join(source, "uv.lock"),
		"the reported sources leave out the lock that pins everything the interpreter imports")
	requireNamesTheSameFile(t, artifact.GetSources(), filepath.Join(source, "pyproject.toml"),
		"the reported sources leave out the project definition")
}

// TestAnUnchangedRestartNamesItsBuildAgain covers the trap in this contract:
// StartResponse CONSUMES the report, so the Start that keeps a running process
// because nothing changed still has to name what that process runs. Reporting
// only at the first start would have a native run refuse a service that is up,
// unchanged and already approved once.
func TestAnUnchangedRestartNamesItsBuildAgain(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, servingApp)
	runtime := uvicornRuntime(t, source, false)
	runtime.serveAddress = fmt.Sprintf("127.0.0.1:%d", runtime.port)

	first, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.NotNil(t, first.GetStatus().GetStartedArtifact())
	running := currentRunner(runtime)

	second, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Same(t, running, currentRunner(runtime),
		"this test only covers the restart that keeps its process; it launched a new one")
	require.Equal(t, runtimev0.StartStatus_STARTED, second.GetStatus().GetState())

	repeated := second.GetStatus().GetStartedArtifact()
	require.NotNil(t, repeated, "a restart that keeps its process must name that process's build again")
	require.Equal(t, first.GetStatus().GetStartedArtifact().GetExecutable(), repeated.GetExecutable())
	require.Equal(t, first.GetStatus().GetStartedArtifact().GetSources(), repeated.GetSources())
}

// TestARestartThatReplacesItsProcessNamesTheNewBuild: a changed request stops
// the process and launches another, and the artifact describes the one now
// running.
func TestARestartThatReplacesItsProcessNamesTheNewBuild(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, servingApp)
	runtime := uvicornRuntime(t, source, false)
	runtime.serveAddress = fmt.Sprintf("127.0.0.1:%d", runtime.port)

	_, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	running := currentRunner(runtime)

	// A different environment is a different process: --reload re-executes the
	// app inside the process it was launched in, which keeps that environment.
	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{
		Overrides: map[string]string{"LOG_LEVEL": "trace"},
	})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.NotSame(t, running, currentRunner(runtime), "a changed request must launch a new process")
	require.NotNil(t, resp.GetStatus().GetStartedArtifact(), "the replacement process must be named too")
	requireDeliverable(t, resp.GetStatus().GetStartedArtifact())
}

// TestAFailedStartNamesNoBuild: a process that never served is not a build to
// approve. Core discards the report on StartError, and nothing here puts one
// back on the attempt that eventually succeeds.
func TestAFailedStartNamesNoBuild(t *testing.T) {
	ctx := context.Background()
	source := uvProject(t, brokenApp)
	runtime := uvicornRuntime(t, source, false)
	runtime.serveAddress = fmt.Sprintf("127.0.0.1:%d", runtime.port)

	resp, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_ERROR, resp.GetStatus().GetState())
	require.Nil(t, resp.GetStatus().GetStartedArtifact(),
		"a start that failed must name no build: the next attempt's report is the only one that describes a running process")

	// And the start that does serve names its own.
	writeApp(t, source, servingApp)
	resp, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.NotNil(t, resp.GetStatus().GetStartedArtifact())
}

// TestAContainerStartNamesNoArtifact: the paths would name files inside the
// image, which mean nothing on the host, and a container run approves that
// image's own digest. Reporting nothing is the contract's answer for an agent
// that cannot name what it ran in terms the consumer can read.
func TestAContainerStartNamesNoArtifact(t *testing.T) {
	runtime := NewRuntime(NewService())
	runtime.Logger = runtime.Wool
	runtime.Base.Runtime.RuntimeContext = resources.NewRuntimeContextContainer()
	runtime.interpreter = "/venv/bin/python"
	require.Nil(t, runtime.startedArtifact(),
		"a container start must name nothing even when an interpreter is known: the path is inside the image")
	require.Empty(t, runtime.launchedInterpreter(context.Background(), nil),
		"a container start must not even ask")
}

// TestAnUnknownInterpreterNamesNoArtifact: Init could not ask uv, or never
// ran. Either way the agent does not know what it is running, and a guess is
// what this contract exists to replace — so it reports nothing and the consumer
// refuses by naming the agent.
func TestAnUnknownInterpreterNamesNoArtifact(t *testing.T) {
	runtime := NewRuntime(NewService())
	runtime.Logger = runtime.Wool
	require.Nil(t, runtime.startedArtifact())
}

// TestAFailedInterpreterProbeStillStarts: the service is up whether or not uv
// could be asked what runs in it, so a probe that fails leaves the interpreter
// unnamed and the start successful — and the start then reports no artifact
// rather than a guess.
func TestAFailedInterpreterProbeStillStarts(t *testing.T) {
	runtime, env := startTestRuntime(t)
	// A fake environment's process produces no output, so the probe finds no
	// answer in it — the shape of a uv that cannot be asked.
	resp, err := runtime.Start(context.Background(), &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, resp.GetStatus().GetState(), resp.GetStatus().GetMessage())
	require.Nil(t, resp.GetStatus().GetStartedArtifact())
	require.Empty(t, runtime.interpreter)
	require.NotEmpty(t, uvicornProcs(env), "the service must still have been launched")
}

// TestTheProbeRunsWithTheEnvironmentTheAppIsLaunchedWith is the fix for the
// hole that probing in Init left: uv resolves the project environment through
// $UV_PROJECT_ENVIRONMENT, which reaches the app from a `--set` and from
// service configuration — neither of which Init has. A probe that could not
// see them would name `<project>/.venv/bin/python` while the app ran somewhere
// else, in an artifact the consumer ACCEPTS because it is absolute and a
// regular file.
func TestTheProbeRunsWithTheEnvironmentTheAppIsLaunchedWith(t *testing.T) {
	runtime, env := startTestRuntime(t)
	const key = "UV_PROJECT_ENVIRONMENT"

	_, err := runtime.Start(context.Background(), &runtimev0.StartRequest{
		Overrides: map[string]string{key: "/tmp/codefly-probe-venv"},
	})
	require.NoError(t, err)

	probe := probeProc(t, env)
	require.Equal(t, []string{"/tmp/codefly-probe-venv"}, processEnv(probe, key),
		"the probe must resolve uv's environment with the overrides the app is launched with")
	require.Equal(t, processEnv(uvicornProcs(env)[0], key), processEnv(probe, key),
		"the probe and the app must resolve against the same project environment")
}

// ── the closure ───────────────────────────────────────────

// uvTree materializes a project tree without running uv: these cases are about
// which files the closure names, not about resolving an environment.
func uvTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return dir
}

// relativeTo renames the closure against the project so a case reads as the
// tree it describes.
func relativeTo(t *testing.T, root string, sources []string) []string {
	t.Helper()
	// Resolved, because the closure names canonical paths and a macOS temp dir
	// is reached as /var and resolves to /private/var — the same tree.
	resolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	root = resolved
	named := make([]string, 0, len(sources))
	for _, source := range sources {
		relative, err := filepath.Rel(root, source)
		require.NoError(t, err)
		named = append(named, filepath.ToSlash(relative))
	}
	slices.Sort(named)
	return named
}

// TestTheClosureIsTheCodeTheLockAndTheManifest pins what the digest is taken
// over: the project definition, the lock, and every file the running process
// can load — including the ones that are not `*.py`, since a changed prompt or
// schema is a changed service. Caches, the virtual environment and the tests
// are left out, each for a reason stated in startedartifact.go.
func TestTheClosureIsTheCodeTheLockAndTheManifest(t *testing.T) {
	service := t.TempDir()
	project := filepath.Join(service, "code")
	require.NoError(t, os.MkdirAll(project, 0o755))
	tree := uvTree(t, map[string]string{
		"code/pyproject.toml":           "[project]\nname = \"svc\"\n",
		"code/uv.lock":                  "version = 1\n",
		"code/src/main.py":              "app = None\n",
		"code/src/admin/router.py":      "\n",
		"code/src/prompts/system.txt":   "you are a service\n",
		"code/src/schema/response.json": "{}\n",
		// Every excluded fixture on these lines sits INSIDE the walked tree,
		// so the assertion depends on the exclusion rather than on the walk's
		// scope: dropping an entry from ephemeralSourceDirectories fails here.
		"code/src/__pycache__/main.cpython.pyc":  "cache\n",
		"code/src/admin/__pycache__/router.pyc":  "cache at depth\n",
		"code/src/.ruff_cache/0/entry":           "cache\n",
		"code/src/.pytest_cache/v/cache/nodeids": "[]\n",
		"code/src/.venv/bin/python":              "a venv inside the tree is still a venv\n",
		// These are outside it, which is what the walk's scope leaves out.
		"code/.venv/bin/python":         "not the interpreter here\n",
		"code/tests/test_admin.py":      "\n",
		"code/openapi/api.swagger.json": "{}\n",
	})

	sources, err := startedSources(tree, filepath.Join(tree, "code"))
	require.NoError(t, err)
	require.Equal(t, []string{
		"code/pyproject.toml",
		"code/src/admin/router.py",
		"code/src/main.py",
		"code/src/prompts/system.txt",
		"code/src/schema/response.json",
		"code/uv.lock",
	}, relativeTo(t, tree, sources))
}

// TestTheClosureNamesTheWorkspaceLockWhereUVResolvesIt: a project that is a
// member of a uv workspace has no lock of its own — uv resolves the root's, and
// so does the image recipe. The artifact names the file that actually pins the
// build, not a path under the project that does not exist.
func TestTheClosureNamesTheWorkspaceLockWhereUVResolvesIt(t *testing.T) {
	tree := uvTree(t, map[string]string{
		"pyproject.toml":      "[tool.uv.workspace]\nmembers = [\"code\"]\n",
		"uv.lock":             "version = 1\n",
		"code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"code/src/main.py":    "app = None\n",
	})

	sources, err := startedSources(tree, filepath.Join(tree, "code"))
	require.NoError(t, err)
	require.Equal(t, []string{
		"code/pyproject.toml",
		"code/src/main.py",
		"pyproject.toml",
		"uv.lock",
	}, relativeTo(t, tree, sources))
}

// TestTheClosureNamesAWorkspaceLockAboveTheService: uv climbs out of the
// service to find the workspace that lists the project, and resolves THAT
// root's lock. The image recipe refuses such a layout, because it would have to
// copy files the service does not own — but the runtime runs where the tree
// already is, and a service that starts and serves must not have its delivery
// refused for a layout uv handles. Reusing the recipe's helper here reported
// nothing at all, which the consumer surfaces as "the agent must implement
// runtime-started-artifact/v1" for an agent that does.
func TestTheClosureNamesAWorkspaceLockAboveTheService(t *testing.T) {
	tree := uvTree(t, map[string]string{
		// The repository root is the workspace root, and the service is below it.
		".git/HEAD":               "ref: refs/heads/main\n",
		"pyproject.toml":          "[tool.uv.workspace]\nmembers = [\"svc/code\"]\n",
		"uv.lock":                 "version = 1\n",
		"svc/code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"svc/code/src/main.py":    "app = None\n",
	})

	sources, err := startedSources(filepath.Join(tree, "svc"), filepath.Join(tree, "svc", "code"))
	require.NoError(t, err, "a workspace root above the service is a layout uv resolves")
	require.Equal(t, []string{
		"pyproject.toml",
		"svc/code/pyproject.toml",
		"svc/code/src/main.py",
		"uv.lock",
	}, relativeTo(t, tree, sources))

	// The image recipe keeps its own refusal: it is the consumer that cannot
	// reach outside its build context.
	_, err = enclosingUVWorkspace(filepath.Join(tree, "svc"), filepath.Join(tree, "svc", "code"))
	require.ErrorContains(t, err, "the image build context cannot include it")
}

// TestAnUnreachableWorkspaceRootIsRefusedBeforeItsLock pins the order of the
// recipe's two refusals, which splitting the discovery out made swappable.
//
// A root the build context cannot reach is refused for THAT reason whether or
// not it has a lock: told to run `uv lock` there instead, a user would do the
// work and be refused anyway.
func TestAnUnreachableWorkspaceRootIsRefusedBeforeItsLock(t *testing.T) {
	tree := uvTree(t, map[string]string{
		".git/HEAD":               "ref: refs/heads/main\n",
		"pyproject.toml":          "[tool.uv.workspace]\nmembers = [\"svc/code\"]\n",
		"svc/code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"svc/code/src/main.py":    "app = None\n",
	})
	service, project := filepath.Join(tree, "svc"), filepath.Join(tree, "svc", "code")

	// No uv.lock anywhere, and the root is out of the service's reach.
	_, err := enclosingUVWorkspace(service, project)
	require.ErrorContains(t, err, "the image build context cannot include it")
	require.NotContains(t, err.Error(), "run `uv lock` there")

	// Reachable but unlocked is the other refusal, and still says so.
	locked := uvTree(t, map[string]string{
		".git/HEAD":               "ref: refs/heads/main\n",
		"svc/pyproject.toml":      "[tool.uv.workspace]\nmembers = [\"code\"]\n",
		"svc/code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"svc/code/src/main.py":    "app = None\n",
	})
	_, err = enclosingUVWorkspace(filepath.Join(locked, "svc"), filepath.Join(locked, "svc", "code"))
	require.ErrorContains(t, err, "run `uv lock` there")

	// And the runtime, which has no reachability rule, names the lock it
	// expected rather than inheriting either refusal.
	_, err = startedSources(service, project)
	require.ErrorContains(t, err, "uv.lock")
	require.NotContains(t, err.Error(), "the image build context cannot include it")
}

// TestTheClosureRefusesToDescribeABuildItCannotRead: a missing lock is not an
// artifact with a gap in it. Naming the rest would have the consumer approve a
// digest that says nothing about the dependencies the process imported, so the
// agent reports no artifact and the consumer refuses by naming it.
func TestTheClosureRefusesToDescribeABuildItCannotRead(t *testing.T) {
	tree := uvTree(t, map[string]string{
		"code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"code/src/main.py":    "app = None\n",
	})
	_, err := startedSources(tree, filepath.Join(tree, "code"))
	require.ErrorContains(t, err, "uv.lock")

	// And with no code to serve, there is likewise nothing to describe.
	bare := uvTree(t, map[string]string{
		"code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"code/uv.lock":        "version = 1\n",
	})
	_, err = startedSources(bare, filepath.Join(bare, "code"))
	require.Error(t, err)
}

// TestTheClosureNamesEveryFileAbsolutely: the consumer resolves no relative
// path, so a project reached by a relative location is still named absolutely.
func TestTheClosureNamesEveryFileAbsolutely(t *testing.T) {
	tree := uvTree(t, map[string]string{
		"code/pyproject.toml": "[project]\nname = \"svc\"\n",
		"code/uv.lock":        "version = 1\n",
		"code/src/main.py":    "app = None\n",
	})
	working, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(working, tree)
	require.NoError(t, err)

	sources, err := startedSources(relative, filepath.Join(relative, "code"))
	require.NoError(t, err)
	for _, source := range sources {
		require.True(t, filepath.IsAbs(source), "%s is not absolute", source)
	}
}

// ── the probe's answer ────────────────────────────────────

// TestTheInterpreterIsReadOutOfUVsOwnOutput: uv writes diagnostics of its own
// onto the stream the probe is read from, so the answer is found by its marker.
// Anything else — no marker, a relative path — is the agent not knowing what it
// ran, which is reported as no artifact rather than as a path.
func TestTheInterpreterIsReadOutOfUVsOwnOutput(t *testing.T) {
	interpreter, err := interpreterFrom(
		"Using CPython 3.14.0\nResolved 12 packages in 3ms\n" + interpreterMarker + "/srv/code/.venv/bin/python\n")
	require.NoError(t, err)
	require.Equal(t, "/srv/code/.venv/bin/python", interpreter)

	_, err = interpreterFrom("Using CPython 3.14.0\n")
	require.ErrorContains(t, err, "named no interpreter")

	_, err = interpreterFrom(interpreterMarker + ".venv/bin/python\n")
	require.ErrorContains(t, err, "not an absolute path")

	_, err = interpreterFrom(interpreterMarker + "\n")
	require.ErrorContains(t, err, "named no interpreter")
}

// TestTheInterpreterProbeLeavesTheEnvironmentAlone observes the property the
// probe's flags are there for, rather than asserting the flags.
//
// The probe runs on the developer's project tree, and `uv.lock` is a file it is
// about to name as part of the build, so `uv run` installing a dependency or
// rewriting that lock would have the probe change what it reports. The flags
// are checked too, because a flag silently dropped in a refactor is how the
// property would be lost.
func TestTheInterpreterProbeLeavesTheEnvironmentAlone(t *testing.T) {
	require.Equal(t, "run", interpreterProbe[0])
	require.Contains(t, interpreterProbe, "--no-sync")
	require.Contains(t, interpreterProbe, "--frozen")

	source := uvProject(t, servingApp)
	runtime := uvicornRuntime(t, source, false)

	// The project is DIRTY on purpose: `uvProject` leaves the lock current, and
	// against a current lock a flagless `uv run` changes nothing either — so a
	// synced fixture would pass this test with both flags deleted. Bumping the
	// version makes uv.lock stale, which is what a plain `uv run` rewrites.
	manifest := filepath.Join(source, "pyproject.toml")
	current, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest,
		[]byte(strings.Replace(string(current), `version = "0.0.0"`, `version = "0.0.1"`, 1)), 0o644))
	before := treeState(t, source)

	interpreter, err := runtime.probeInterpreter(context.Background(), runtime.runnerEnvironment, nil)
	require.NoError(t, err)
	require.NotEmpty(t, interpreter)

	require.Equal(t, before, treeState(t, source),
		"the probe installed into the environment or rewrote the lock it is about to name as part of the build")
}

// treeState is every file under root with its size and modification time,
// except the bytecode caches.
//
// Those are excluded because ANY python invocation writes them — the probe
// imports `sys` and CPython caches the venv's own `_virtualenv` module — so
// they are not evidence that uv installed or re-locked anything, which is what
// the assertion above is about. It is the same reason the closure leaves
// `__pycache__` out: derived, and written by running code rather than by a
// developer.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".pyc") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			// A file uv replaced under the walk is itself a change.
			state[path] = "vanished"
			return nil
		}
		state[path] = fmt.Sprintf("%d@%d", info.Size(), info.ModTime().UnixNano())
		return nil
	}))
	return state
}

// TestAdvertisementAdoptsRuntimeStartedArtifact: the capability says this agent
// calls ReportStartedArtifact, which is what lets a consumer read the build off
// the StartStatus instead of deriving a path from the python layout. An agent
// that stopped reporting would have to stop advertising, so the two are held
// together here.
func TestAdvertisementAdoptsRuntimeStartedArtifact(t *testing.T) {
	info, err := NewService().GetAgentInformation(context.Background(), &agentv0.AgentInformationRequest{})
	require.NoError(t, err)
	require.Contains(t, info.GetContract().GetCapabilities(), contract.RuntimeStartedArtifact)
	require.NoError(t, contract.Check(info.GetContract(), contract.RuntimeStartedArtifact))
}
