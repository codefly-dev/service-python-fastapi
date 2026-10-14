package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
)

// What a native start ran — core's runtime-started-artifact/v1.
//
// A consumer that pins the build a running process is at cannot derive one for
// this agent: there is no binary this agent built, and the interpreter it runs
// says nothing about the service. So the agent names it, and the CLI's native
// run approves exactly what is named — refusing a workload whose agent reports
// nothing rather than guessing a path from the python layout (core#796,
// cli#993).
//
// THE PROGRAM is the interpreter `uv run` will execute the app with, and it is
// asked of uv rather than derived. `uv run uvicorn src.main:app` resolves the
// project environment through $UV_PROJECT_ENVIRONMENT, a `.python-version`, the
// lock's requires-python and uv's own configuration — and that variable reaches
// the process from the developer's own environment, from a `uv.toml`, from a
// flake of theirs, and from this run's `--set` and service configuration. So
// `<project>/.venv/bin/python` is a convention this agent must not encode.
// Asking uv, in the environment the app is launched with, answers for every
// backend with one mechanism: a native run, and a nix run where uv and python
// both come from the devShell.
//
// THE SOURCES are what uv hands that interpreter:
//
//   - the project definition, `pyproject.toml` — its dependencies, its
//     `[tool.uv]` settings and its `[tool.uv.sources]` all decide what is
//     installed and therefore what the app imports;
//   - the lock pinning them, `uv.lock` — at the project, or at the uv
//     workspace root when the project is a member of one, which is where uv
//     itself resolves it from (see uvworkspace.go). A member's root is read
//     the same way the image recipe reads it, so the runtime describes the
//     build the recipe would build;
//   - the service's own code tree, `src/` — every regular file in it, not only
//     `*.py`. The entrypoint `src.main:app` is in there (as `src/main.py`, or
//     as `src/main/__init__.py` — naming the tree covers both), and so is a
//     prompt, a template or a schema the app loads at runtime. A digest that
//     tracked only `*.py` would call a changed prompt the same build.
//
// WHAT IS LEFT OUT, and why each exclusion is the honest answer rather than a
// convenience:
//
//   - `.venv/` — uv's materialization of the lock, not the pin. The lock and
//     the interpreter above already identify it, while its contents are
//     rewritten by every `uv sync` and carry absolute paths of their own: a
//     digest over them would move for a tree nobody edited and would still not
//     answer what the developer changed. The interpreter inside it IS reported,
//     because it is what runs.
//   - `__pycache__`, `.pytest_cache`, `.ruff_cache`, `.mypy_cache` and the rest
//     of ephemeralSourceDirectories — derived from the `.py` files already
//     named, and written by the running process itself, so including them
//     would make two starts of identical code two builds.
//   - `tests/` — never imported by the serving process.
//   - `openapi/api.swagger.json` — generated FROM `src/` by the service, read
//     by dependents rather than by the app.
//   - `uv` itself, the launcher this agent spawns. It resolves the environment
//     and then steps aside; against an existing lock a different uv produces
//     the same venv, so the process it leaves behind is the one the
//     interpreter, the lock and the code above already name. Putting its bytes
//     in the closure would instead make every developer's uv upgrade a new
//     build for a service nobody touched.
//
// Two limits are stated rather than papered over. A `[tool.uv.sources]` path
// dependency's own content is not in the closure: its declaration is (in
// pyproject.toml and uv.lock), but editing that library's code is a build this
// digest cannot tell apart until uv re-locks. And a container start reports
// nothing at all — the paths would name files inside the image, which say
// nothing on the host, and a container run approves an OCI image digest
// instead.

// interpreterMarker prefixes the one line the probe reads out of uv's output.
// uv shares the stream with diagnostics of its own ("Using CPython 3.14.0"), so
// the line is found by this prefix rather than by being the only one.
const interpreterMarker = "codefly-started-interpreter="

// interpreterProbe asks the interpreter for its own absolute path.
//
// `--no-sync` and `--frozen` state what the probe must not do, each covering a
// different mutation `uv run` performs by default: no dependency is installed
// into the environment, and the lock is not rewritten. The probe runs on the
// developer's own project tree, moments before the app does, and `uv.lock` is a
// file it is about to NAME as part of the build — a probe that re-locked would
// be changing the thing it reports. Observing costs nothing; it must not also
// decide.
//
// What they do NOT prevent is uv CREATING the project environment when there is
// none — observed on uv 0.12.23, which prints "Creating virtual environment at:
// .venv" and then answers. That is harmless here and the answer is still the
// right one: the environment it creates is at the path `uv run uvicorn` would
// create moments later, so the interpreter named is the interpreter that runs.
// It is called out because the flags read like read-only and are not.
var interpreterProbe = []string{"run", "--no-sync", "--frozen", "python", "-c",
	"import sys; print('" + interpreterMarker + "' + sys.executable)"}

// interpreterProbeTimeout bounds the probe. On a synced project it returns in
// well under a second; a bound exists because the probe sits on the path to
// launching the service, so a wedged uv must degrade into "the agent could not
// name what it ran" rather than into a service that never starts.
const interpreterProbeTimeout = 60 * time.Second

// launchedInterpreter is the interpreter the process being launched will run,
// or "" when the agent cannot name it.
//
// It is asked at the launch, with that launch's own environment, because the
// answer is a property of the launch and not of the agent's install. uv
// resolves the project environment through $UV_PROJECT_ENVIRONMENT, and that
// variable reaches the app from the StartRequest's overrides and from the
// service's configuration — neither of which exists when Init builds the
// runner environment. Probing in Init would answer for the agent's own
// environment instead, and a `--set UV_PROJECT_ENVIRONMENT=...` would then
// have this agent name an interpreter nothing ran, in an artifact the consumer
// accepts: absolute, a regular file, and wrong.
//
// A probe that fails leaves the interpreter unnamed and the start successful —
// the service is up either way, and a start that cannot say what it runs
// reports no artifact rather than refusing to run.
func (s *Runtime) launchedInterpreter(ctx context.Context, startEnvs []*resources.EnvironmentVariable) string {
	if s.runnerEnvironment == nil || s.Base.Runtime.IsContainerRuntime() {
		return ""
	}
	interpreter, err := s.probeInterpreter(ctx, s.runnerEnvironment, startEnvs)
	if err != nil {
		// Error, not Warn: the only thing the user is otherwise shown is the
		// consumer refusing because "the agent must implement
		// runtime-started-artifact/v1" — which this agent does, so the real
		// cause has to reach the same log the refusal is read in.
		s.Wool.Error("cannot name the interpreter this start runs, so it reports no started artifact", wool.ErrField(err))
		return ""
	}
	return interpreter
}

// startedArtifact is what the running process is, or nil when the agent cannot
// name it.
//
// The interpreter comes from the launch that produced the live process; the
// sources are read now. They are gathered differently on purpose: the
// interpreter is fixed by the launch's environment, which a Start that keeps
// the process does not change, while the source closure is a set of NAMES the
// consumer digests from disk at delivery — so naming the files that exist is
// what lets a build be digested at all, and a name kept from a launch whose
// file has since been deleted would fail the delivery of a service that is
// running. The window this leaves is real and is documented rather than
// papered over: between an edit and the restart that picks it up, the digest
// the consumer takes describes the tree rather than the process's loaded
// image. Only a restart closes it, and `--reload` is what performs one.
//
// Nil is a deliberate answer, not a failure to report: the contract has an
// agent that cannot name the file it ran report no artifact, so that a consumer
// refuses by naming the agent instead of approving a build nobody confirmed.
// The start itself still succeeds — the service is up, and describing it is a
// separate question from running it.
func (s *Runtime) startedArtifact() *runtimev0.StartedArtifact {
	if s.Base.Runtime.IsContainerRuntime() {
		// A container start's paths name files in the image; the run approves
		// that image's digest, not a closure on this host.
		return nil
	}
	if s.interpreter == "" {
		// Init could not name it, or never ran. Either way this agent does not
		// know what it is running, which is reported as nothing.
		return nil
	}
	sources, err := startedSources(s.Location, s.Service.SourceLocation)
	if err != nil {
		s.Wool.Warn("cannot name the sources this start ran, so it reports no started artifact", wool.ErrField(err))
		return nil
	}
	return &runtimev0.StartedArtifact{Executable: s.interpreter, Sources: sources}
}

// probeInterpreter is the absolute path of the interpreter `uv run` executes
// the app with, as uv resolves it in the environment the app is launched in.
func (s *Runtime) probeInterpreter(ctx context.Context, env runners.RunnerEnvironment, startEnvs []*resources.EnvironmentVariable) (string, error) {
	proc, err := env.NewProcess("uv", interpreterProbe...)
	if err != nil {
		return "", fmt.Errorf("cannot create the interpreter probe: %w", err)
	}
	var out bytes.Buffer
	proc.WithOutput(&out)
	proc.WithDir(s.Service.SourceLocation)
	// The same environment the app is launched with, so the probe resolves the
	// project environment the app will run out of rather than the agent's.
	proc.WithEnvironmentVariables(ctx, startEnvs...)

	ctx, cancel := context.WithTimeout(ctx, interpreterProbeTimeout)
	defer cancel()
	if err := proc.Run(ctx); err != nil {
		return "", fmt.Errorf("the interpreter probe failed: %w\n%s", err, out.String())
	}
	return interpreterFrom(out.String())
}

// interpreterFrom reads the probe's answer out of uv's output.
func interpreterFrom(output string) (string, error) {
	for line := range strings.SplitSeq(output, "\n") {
		_, reported, found := strings.Cut(line, interpreterMarker)
		if !found {
			continue
		}
		interpreter := strings.TrimSpace(reported)
		if interpreter == "" {
			continue
		}
		if !filepath.IsAbs(interpreter) {
			// Every path in the artifact is absolute: a consumer resolves none
			// of them, because its working directory is the user's terminal and
			// has nothing to do with what this agent ran.
			return "", fmt.Errorf("the interpreter probe reported %q, which is not an absolute path", interpreter)
		}
		return interpreter, nil
	}
	return "", fmt.Errorf("the interpreter probe named no interpreter:\n%s", output)
}

// startedSources are the files uv hands the interpreter, as absolute paths.
//
// serviceDir is the service root, sourceDir its python project (`<service>/code`).
func startedSources(serviceDir, sourceDir string) ([]string, error) {
	serviceDir, err := canonical(serviceDir)
	if err != nil {
		return nil, err
	}
	sourceDir, err = canonical(sourceDir)
	if err != nil {
		return nil, err
	}

	// The project's own definition, always.
	sources := []string{filepath.Join(sourceDir, "pyproject.toml")}

	// The lock uv resolves against: the project's, or the workspace root's when
	// the project is a member of one. The root's own pyproject.toml comes with
	// it — it carries the `[tool.uv.workspace]` membership and any shared
	// `[tool.uv.sources]`, so a change there changes what is installed.
	//
	// uvWorkspaceRoot, not enclosingUVWorkspace: that one is the image recipe's
	// view and refuses a root above the service, because the recipe would have
	// to copy it. The runtime runs where the tree already is, so a root above
	// the service is a layout uv resolves and this agent runs — describing it
	// is what keeps a working service deliverable.
	lockDir := sourceDir
	root, err := uvWorkspaceRoot(serviceDir, sourceDir)
	if err != nil {
		return nil, err
	}
	if root != "" {
		lockDir = root
		sources = append(sources, filepath.Join(lockDir, "pyproject.toml"))
	}
	sources = append(sources, filepath.Join(lockDir, "uv.lock"))

	// Each of the above identifies the build, so a missing one is not an
	// artifact with a gap in it — it is an artifact this agent cannot name.
	for _, source := range sources {
		info, err := os.Stat(source)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s, which this start resolved its dependencies from: %w", source, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", source)
		}
	}

	tree, err := codeTree(filepath.Join(sourceDir, "src"))
	if err != nil {
		return nil, err
	}
	return append(sources, tree...), nil
}

// canonical is dir as one absolute, symlink-resolved spelling.
//
// One spelling, because the closure mixes paths built from this root with the
// workspace root uv resolution returns, and a report naming the same tree as
// both /var/... and /private/var/... is confusing to read even where the
// consumer resolves it anyway. Directories only: the file's own link is never
// followed, so an interpreter at `.venv/bin/python` keeps the name the
// workspace holds it under rather than the one it points at — which is the
// name the consumer digests it by (cli#989).
func canonical(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		// Not yet on disk, or unreadable: the absolute spelling is the only
		// one there is, and the stats below report what is actually missing.
		return absolute, nil
	}
	return resolved, nil
}

// codeTree is every regular file under root that the running process can load,
// in lexical order.
//
// A tool cache or a virtual environment found in there is skipped
// (ephemeralSourceDirectories, shared with the image build context, which
// excludes them for the same reason: they are outputs of whatever the developer
// last ran). Anything that is not a regular file — a symlinked directory, a
// socket — is skipped too: the contract has the agent name files, never a
// directory for the consumer to walk, so one would fail the consumer's read
// rather than describe the build.
func codeTree(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && ephemeralSourceDirectories[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		// Stat, not the DirEntry's own type: a symlink to a file is a file the
		// process loads, and a symlink to a directory is not.
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cannot read the code tree at %s, which this start served from: %w", root, err)
	}
	if len(files) == 0 {
		return nil, errors.New("the code tree at " + root + " holds no file the app could have been served from")
	}
	return files, nil
}
