//go:build requirenix

package main

import (
	"bytes"
	"context"
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
