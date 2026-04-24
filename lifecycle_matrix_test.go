package main_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/runners/testmatrix"
)

// TestPythonFastAPILifecycle_Matrix exercises python-fastapi's parity
// across native, nix, and docker by running `python3 --version` in each.
// Uses the same uv-bundled image the runtime uses in container mode
// (main.go `runtimeImage`). Nix + native rely on host's python3 via PATH
// or flake.nix respectively.
func TestPythonFastAPILifecycle_Matrix(t *testing.T) {
	dir, err := os.MkdirTemp("", "pyfastapi-matrix-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)

	// python:3.12-slim has /usr/local/bin/python3 directly, no entrypoint
	// wrapper. The runtime (main.go) uses the uv-bundled image for actual
	// service execution; this matrix test just validates toolchain reach
	// across backends, so a plain python image is the right fit.
	img := &resources.DockerImage{Name: "python", Tag: "3.12-slim"}

	testmatrix.ForEachEnvironment(t, dir,
		func(t *testing.T, env runners.RunnerEnvironment) {
			proc, err := env.NewProcess("python3", "--version")
			if err != nil {
				t.Fatalf("NewProcess: %v", err)
			}
			var buf bytes.Buffer
			proc.WithOutput(&buf)
			if err := proc.Run(context.Background()); err != nil {
				t.Fatalf("python3 --version failed: %v", err)
			}
			out := buf.String()
			if !strings.Contains(strings.ToLower(out), "python") {
				t.Fatalf("expected Python version string, got %q", out)
			}
		},
		testmatrix.WithDockerImage(img),
	)
}
