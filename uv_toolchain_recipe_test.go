package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildConstraintProject is a project declaring a `[tool.uv]` key that only a
// current uv knows. It is the shape that resolved on the developer's machine
// and failed inside the builder stage, reported there as
//
//	TOML parse error at line N, column 1
//	unknown field `build-constraint-dependencies`
//
// because the uv the builder stage ran was the one the base image was built
// with, not the one the project was written against.
const buildConstraintProject = `[project]
name = "example-worker"
version = "0.1.0"
requires-python = ">=3.10"
dependencies = []

[tool.uv]
package = false
build-constraint-dependencies = ["setuptools==80.9.0"]
`

// TestRecipePinsTheUVItResolvesWith holds the toolchain half of the recipe
// contract: the version of the tool that reads pyproject.toml and uv.lock is
// stated by the recipe, at an exact version, and copied in before anything
// resolves — not inherited from whichever uv the base image happens to carry.
func TestRecipePinsTheUVItResolvesWith(t *testing.T) {
	require.Regexp(t, regexp.MustCompile(`^\d+\.\d+\.\d+$`), uvVersion,
		"uv must be pinned to an exact version; a range or a moving tag is not a reproducible recipe")
	require.Equal(t, "ghcr.io/astral-sh/uv:"+uvVersion, uvImage)

	builder, _ := createdBuilder(t)
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	dockerfile := dockerfileOf(t, outputDir, plan)

	copied := fmt.Sprintf("COPY --from=%s /uv /usr/local/bin/uv", uvImage)
	require.Contains(t, dockerfile, copied)
	require.Less(t, strings.Index(dockerfile, copied), strings.Index(dockerfile, "uv sync --frozen"),
		"the pinned uv must be in place before the recipe resolves anything with it")
}

// TestRecipeBuilderStageResolvesCurrentUVKeys is the behavioural half, and the
// regression for the failure this pin exists for: a project declaring a
// `[tool.uv]` key introduced after the base image was built resolves in the
// builder stage the recipe emits, and the uv that resolved it is the pinned
// one.
//
// It also records what the base image's own uv makes of the same project, so
// the evidence for the pin is in the run rather than in a claim about it.
func TestRecipeBuilderStageResolvesCurrentUVKeys(t *testing.T) {
	for _, tool := range []string{"docker", "uv"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: the builder stage cannot be built", tool)
		}
	}

	builder, _ := createdBuilder(t)
	code := filepath.Join(builder.Location, "code")
	writeFile(t, filepath.Join(code, "pyproject.toml"), buildConstraintProject)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()

	lock := exec.CommandContext(ctx, "uv", "lock")
	lock.Dir = code
	output, err := lock.CombinedOutput()
	require.NoError(t, err, "uv lock: %s", output)

	// What the base image's uv makes of the same project, named rather than
	// assumed. This is evidence, not a gate: a newer base image knowing the key
	// is an improvement, never a regression of the pin.
	base := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", code+":/probe", "-w", "/probe",
		runtimeImage.FullName(), "sh", "-c", "uv --version; uv sync --frozen --no-dev --no-install-project 2>&1 | head -20")
	probe, _ := base.CombinedOutput()
	t.Logf("base image %s resolving the same project:\n%s", runtimeImage.FullName(), probe)

	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)

	tag := fmt.Sprintf("codefly-fastapi-uv-pin-test:%d", time.Now().UnixMilli())
	build := exec.CommandContext(ctx, "docker", "build", "--target", "builder",
		"-f", filepath.Join(outputDir, plan.GetRecipes()[0].GetDockerfile()),
		"-t", tag, builder.Location)
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		// docker is installed and the project resolves on this host, so this is
		// the builder stage refusing the project — the failure the pin is for.
		t.Fatalf("the builder stage did not resolve a project using current uv keys: %v\n%s", buildErr, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	version, err := exec.CommandContext(ctx, "docker", "run", "--rm", tag, "uv", "--version").CombinedOutput()
	require.NoError(t, err, "%s", version)
	require.Contains(t, string(version), uvVersion,
		"the builder stage resolved with a uv the recipe does not name")
}
