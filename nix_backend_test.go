//go:build requirenix

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/runners/testmatrix"
	"github.com/stretchr/testify/require"
)

// TestNixBackendToolchain exercises the backend main.go advertises as
// `Nix: true`, the way runtime.go reaches it: provision the embedded flake
// into a source dir that has none, materialize its devShell, and resolve the
// binaries the FastAPI runtime shells out to.
//
// Only("nix") makes a host without nix a failure rather than a skip, so this
// lives behind the `requirenix` tag: the default suite stays portable, and
// ci.yml's `nix` job is the one that asserts the backend exists.
func TestNixBackendToolchain(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ensureNixFlake(dir))

	testmatrix.ForEachEnvironment(t, dir,
		func(t *testing.T, env runners.RunnerEnvironment) {
			for _, bin := range []string{"python3", "uv"} {
				proc, err := env.NewProcess(bin, "--version")
				require.NoError(t, err, "NewProcess %s", bin)
				var buf bytes.Buffer
				proc.WithOutput(&buf)
				require.NoError(t, proc.Run(context.Background()), "%s --version", bin)
				require.NotEmpty(t, strings.TrimSpace(buf.String()), "%s --version printed nothing", bin)
			}
		},
		testmatrix.Only("nix"),
	)
}

// TestNixBackendNamesTheInterpreterItWouldRun closes the backend half of
// runtime-started-artifact/v1.
//
// The CLI's native delivery no-ops only for a container runtime, so a nix run
// goes through it and needs a report — and under nix both `uv` and the python
// it resolves come from the devShell, not from the host. The claim that asking
// uv "answers for every backend" is therefore asserted here rather than
// reasoned about: the probe runs through the materialized devShell and must
// name an absolute, regular file, which is what the consumer digests.
func TestNixBackendNamesTheInterpreterItWouldRun(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	// Dependency-free: the interpreter is what is under test, and resolving a
	// wheel would only make the nix job slower.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"),
		[]byte("[project]\nname = \"nix-probe\"\nversion = \"0.0.0\"\nrequires-python = \">=3.10\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "main.py"), []byte("app = None\n"), 0o644))
	require.NoError(t, ensureNixFlake(dir))

	runtime := NewRuntime(NewService())
	runtime.Location = dir
	runtime.Logger = runtime.Wool
	runtime.Service.SourceLocation = dir

	testmatrix.ForEachEnvironment(t, dir,
		func(t *testing.T, env runners.RunnerEnvironment) {
			// The environment the probe observes, as Init leaves it.
			sync, err := env.NewProcess("uv", "sync")
			require.NoError(t, err)
			sync.WithDir(dir)
			var synced bytes.Buffer
			sync.WithOutput(&synced)
			require.NoError(t, sync.Run(context.Background()), "uv sync in the devShell: %s", synced.String())

			interpreter, err := runtime.probeInterpreter(context.Background(), env, nil)
			require.NoError(t, err)
			require.True(t, filepath.IsAbs(interpreter), "%s is not absolute", interpreter)
			info, err := os.Stat(interpreter)
			require.NoError(t, err, "the consumer could not read %s", interpreter)
			require.True(t, info.Mode().IsRegular(), "%s is not a regular file", interpreter)
		},
		testmatrix.Only("nix"),
	)
}
