package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// writeFile writes content at path, creating parents.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// siblingLibrary writes a minimal installable project at directory — the shape
// a second codefly service's Python project has when a first one depends on it
// by path.
func siblingLibrary(t *testing.T, directory string) {
	t.Helper()
	writeFile(t, filepath.Join(directory, "pyproject.toml"),
		"[project]\nname = \"example-lib\"\nversion = \"0.1.0\"\nrequires-python = \">=3.10\"\ndependencies = []\n")
	writeFile(t, filepath.Join(directory, "src", "__init__.py"), "NAME = \"example-lib\"\n")
}

// pathSourceProject replaces the scaffold's project with one whose only
// dependency is resolved from declared, so the recipe's behaviour is what the
// test exercises and not a dependency download.
func pathSourceProject(t *testing.T, builder *Builder, declared string) {
	t.Helper()
	writeFile(t, filepath.Join(builder.Location, "code", "pyproject.toml"), fmt.Sprintf(`[project]
name = "example-worker"
version = "0.1.0"
requires-python = ">=3.10"
dependencies = ["example-lib"]

[tool.uv]
package = false

[tool.uv.sources]
example-lib = { path = %q }
`, declared))
}

// escapingSourceBuilder is a created service whose project depends on a library
// in a sibling service of the same repository — the layout the default context,
// which is the service directory and nothing beside it, cannot build. It
// reports whether the project could be locked, which needs uv on the host.
func escapingSourceBuilder(t *testing.T) (*Builder, bool) {
	t.Helper()
	builder, _ := createdBuilder(t)
	siblingLibrary(t, filepath.Join(filepath.Dir(builder.Location), "lib", "code"))
	pathSourceProject(t, builder, "../../lib/code")
	if _, err := exec.LookPath("uv"); err != nil {
		return builder, false
	}
	lock := exec.Command("uv", "lock")
	lock.Dir = filepath.Join(builder.Location, "code")
	output, err := lock.CombinedOutput()
	// uv is installed, so a failure here is a broken fixture, not an
	// unequipped host.
	require.NoError(t, err, "uv lock: %s", output)
	return builder, true
}

// emitRecipe runs Build into a fresh directory and returns the plan and that
// directory.
func emitRecipe(t *testing.T, builder *Builder, outputDir string) *builderv0.DockerBuildPlan {
	t.Helper()
	resp, err := builder.Build(t.Context(), recipeBuildRequest(outputDir))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan)
	return plan
}

// dockerfileOf reads the Dockerfile a plan's single recipe names.
func dockerfileOf(t *testing.T, outputDir string, plan *builderv0.DockerBuildPlan) string {
	t.Helper()
	require.Len(t, plan.GetRecipes(), 1)
	content, err := os.ReadFile(filepath.Join(outputDir, plan.GetRecipes()[0].GetDockerfile()))
	require.NoError(t, err)
	return string(content)
}

// TestBuildRecipeCarriesEscapingUVPathSource is the defect this recipe had: a
// project whose `[tool.uv.sources]` path leaves its own directory emitted a
// context without the directory it names, and the builder stage failed on
// `uv sync --frozen`, while the identical resolution succeeded on the
// developer's machine.
//
// It proves both halves against the real tool: the context as the old recipe
// built it — the service directory alone — does not resolve the path source,
// and the context this recipe assembles does, in a location where the original
// relative path leads nowhere.
func TestBuildRecipeCarriesEscapingUVPathSource(t *testing.T) {
	builder, locked := escapingSourceBuilder(t)
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)

	// The whole destination is this build's output, so the plan claims all of
	// it and the CLI roots the docker context there instead of at the service.
	require.Equal(t, builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_TREE, plan.GetScope())
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))

	service, err := filepath.Rel(repositoryOf(t, builder.Location), evalSymlinks(t, builder.Location))
	require.NoError(t, err)
	library := filepath.Join(filepath.Dir(service), "lib", "code")
	for _, rel := range []string{
		filepath.Join("context", service, "code", "pyproject.toml"),
		filepath.Join("context", service, "service.codefly.yaml"),
		filepath.Join("context", library, "pyproject.toml"),
		filepath.Join("context", library, "src", "__init__.py"),
	} {
		require.FileExists(t, filepath.Join(outputDir, rel))
	}

	dockerfile := dockerfileOf(t, outputDir, plan)
	slashedService := filepath.ToSlash(service)
	slashedLibrary := filepath.ToSlash(library)
	require.Contains(t, dockerfile, "WORKDIR /src/"+slashedService+"/code")
	require.Contains(t, dockerfile,
		fmt.Sprintf("COPY context/%s/code/pyproject.toml context/%s/code/uv.lock ./", slashedService, slashedService))
	require.Contains(t, dockerfile,
		fmt.Sprintf("COPY context/%s /src/%s", slashedLibrary, slashedLibrary))
	require.Contains(t, dockerfile,
		fmt.Sprintf("COPY --chown=appuser context/%s/service.codefly.yaml .", slashedService))
	require.Contains(t, dockerfile,
		fmt.Sprintf("COPY --chown=appuser context/%s/code/src code/src", slashedService))

	if !locked {
		t.Skip("uv is not installed: the emitted context cannot be resolved against the real tool")
	}
	require.FileExists(t, filepath.Join(outputDir, "context", service, "code", "uv.lock"))

	// The old recipe, reconstructed: the service directory alone, in a place
	// where the path source's original relative path leads nowhere.
	old := filepath.Join(t.TempDir(), "app")
	copyTree(t, filepath.Join(outputDir, "context", service, "code"), old)
	output, err := runUVSync(t, old)
	require.Error(t, err, "the old context must not resolve its path source, got:\n%s", output)
	require.Contains(t, strings.ToLower(output), "lib/code",
		"expected the path-resolution failure this fix is for, got:\n%s", output)

	// The emitted context, placed the way the builder stage places it.
	source := filepath.Join(t.TempDir(), "src")
	copyTree(t, filepath.Join(outputDir, "context"), source)
	output, err = runUVSync(t, filepath.Join(source, service, "code"))
	require.NoError(t, err, "uv sync in the emitted context:\n%s", output)
}

// TestBuildRecipeWithoutEscapingPathSourceIsUnchanged holds the no-op case: a
// service whose project stays inside its own directory emits exactly the recipe
// it emitted before — only the Dockerfile and its ignore, claimed as such, with
// the context left to the caller's service directory.
func TestBuildRecipeWithoutEscapingPathSourceIsUnchanged(t *testing.T) {
	builder, _ := createdBuilder(t)
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)

	require.Equal(t, builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_EMITTED, plan.GetScope())
	require.NoDirExists(t, filepath.Join(outputDir, "context"))

	dockerfile := dockerfileOf(t, outputDir, plan)
	require.Contains(t, dockerfile, "WORKDIR /app")
	require.Contains(t, dockerfile, "COPY code/pyproject.toml code/uv.lock ./")
	require.Contains(t, dockerfile, "COPY --chown=appuser service.codefly.yaml .")
	require.Contains(t, dockerfile, "COPY --chown=appuser code/src code/src")
	require.NotContains(t, dockerfile, "context/")
}

// TestBuildRecipeCarryIsIdempotentInAReusedDirectory holds the layout the CLI
// hands the agent: one recipe directory per service, reused from build to
// build. A tree an earlier emission carried must not outlive the declaration
// that asked for it, and the second emission must not trip over its own output.
func TestBuildRecipeCarryIsIdempotentInAReusedDirectory(t *testing.T) {
	builder, _ := escapingSourceBuilder(t)
	outputDir := t.TempDir()
	stale := filepath.Join(outputDir, "context", "gone", "leftover.py")

	for attempt := 1; attempt <= 2; attempt++ {
		plan := emitRecipe(t, builder, outputDir)
		require.Equal(t, builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_TREE, plan.GetScope(),
			"emission %d", attempt)
		require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan), "emission %d", attempt)
		if attempt == 1 {
			writeFile(t, stale, "# carried for a declaration that no longer exists\n")
		}
	}
	require.NoFileExists(t, stale, "a tree no declaration asks for survived re-emission")
}

// TestBuildRecipeRefusesPathSourceOutsideRepository proves a path source that
// leaves the repository is reported, naming the declaration. Copying it would
// put one machine's filesystem into an image; skipping it silently would build
// the image against a different dependency than the developer builds against.
func TestBuildRecipeRefusesPathSourceOutsideRepository(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "lib", "code")
	siblingLibrary(t, outside)
	builder, _ := createdBuilder(t)
	pathSourceProject(t, builder, outside)

	resp, err := builder.Build(t.Context(), recipeBuildRequest(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
	require.Contains(t, resp.GetState().GetMessage(), "[tool.uv.sources] example-lib")
	require.Contains(t, resp.GetState().GetMessage(), "outside the repository")
}

// TestBuildRecipeReportsAMissingPathSource proves a declaration naming a
// directory that is not there is reported as such, rather than emitting a
// recipe that fails inside the builder stage.
func TestBuildRecipeReportsAMissingPathSource(t *testing.T) {
	builder, _ := createdBuilder(t)
	pathSourceProject(t, builder, "../../absent/code")

	resp, err := builder.Build(t.Context(), recipeBuildRequest(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, resp.GetState().GetState())
	require.Contains(t, resp.GetState().GetMessage(), "does not exist")
}

// runUVSync runs the builder stage's install command in directory, with its own
// environment so the test never writes into a project's own venv.
func runUVSync(t *testing.T, directory string) (string, error) {
	t.Helper()
	command := exec.Command("uv", "sync", "--frozen", "--no-dev", "--no-install-project")
	command.Dir = directory
	command.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+t.TempDir())
	output, err := command.CombinedOutput()
	return string(output), err
}

// copyTree copies src to dst the way the Dockerfile's COPY does.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	output, err := exec.Command("cp", "-R", src, dst).CombinedOutput()
	require.NoError(t, err, "copy %s to %s: %s", src, dst, output)
}

func evalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}

func repositoryOf(t *testing.T, path string) string {
	t.Helper()
	root, err := repositoryRoot(evalSymlinks(t, path))
	require.NoError(t, err)
	return root
}

// TestAssembledContextBuildsTheImage is the end-to-end half: the context this
// recipe assembles is built by docker from the root the plan's TREE scope tells
// the caller to build, and the image that comes out carries the dependency the
// escaping path source names. A Dockerfile whose COPY paths were right on paper
// and wrong against the assembled tree fails here and nowhere else.
func TestAssembledContextBuildsTheImage(t *testing.T) {
	for _, tool := range []string{"docker", "uv"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: the assembled context cannot be built", tool)
		}
	}
	builder, locked := escapingSourceBuilder(t)
	require.True(t, locked)
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	require.Equal(t, builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_TREE, plan.GetScope())

	tag := fmt.Sprintf("codefly-fastapi-uv-context-test:%d", time.Now().UnixMilli())
	// The context root a TREE plan declares is the assembled output directory,
	// not the service directory — which is what the caller resolves from the
	// scope, and what this build has to be exercised against.
	build := exec.CommandContext(t.Context(), "docker", "build",
		"-f", filepath.Join(outputDir, plan.GetRecipes()[0].GetDockerfile()),
		"-t", tag, outputDir)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the assembled context did not build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	installed, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", tag,
		"python", "-c", "import importlib.metadata as m; print(m.version('example-lib'))").CombinedOutput()
	require.NoError(t, err, "%s", installed)
	require.Contains(t, string(installed), "0.1.0",
		"the image does not carry the dependency the path source names")
}
