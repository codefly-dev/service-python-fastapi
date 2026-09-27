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

// workspaceRequiresPython is outside the Python the base image carries, so the
// image build has to use the interpreter uv installs for the project, as the
// host does.
const workspaceRequiresPython = ">=3.11,<3.12"

// uvWorkspaceService lays a service out the way a uv workspace member project
// is: the service directory is the workspace root and holds the one uv.lock,
// `code/` is a member depending on a library member `{ workspace = true }` —
// which uv honours only under an ancestor root — and `code/` has no lock of
// its own.
func uvWorkspaceService(t *testing.T, serviceDir, requiresPython string) {
	t.Helper()
	_ = os.Remove(filepath.Join(serviceDir, "code", "uv.lock"))
	writeFile(t, filepath.Join(serviceDir, "pyproject.toml"), `[tool.uv.workspace]
members = ["code", "packages/*"]
exclude = ["packages/scratch"]
`)
	writeFile(t, filepath.Join(serviceDir, "packages", "greeting", "pyproject.toml"), fmt.Sprintf(`[project]
name = "greeting"
version = "0.1.0"
requires-python = %q
dependencies = []

[build-system]
requires = ["uv_build>=0.8,<0.13"]
build-backend = "uv_build"
`, requiresPython))
	writeFile(t, filepath.Join(serviceDir, "packages", "greeting", "src", "greeting", "__init__.py"), "TEXT = \"hello from a workspace member\"\n")
	writeFile(t, filepath.Join(serviceDir, "code", "pyproject.toml"), fmt.Sprintf(`[project]
name = "example-service"
version = "0.1.0"
requires-python = %q
dependencies = ["greeting"]

[tool.uv]
package = false

[tool.uv.sources]
greeting = { workspace = true }
`, requiresPython))
}

func lockWorkspace(t *testing.T, serviceDir string) bool {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		return false
	}
	lock := exec.Command("uv", "lock")
	lock.Dir = serviceDir
	output, err := lock.CombinedOutput()
	require.NoError(t, err, "uv lock: %s", output)
	return true
}

func TestEnclosingUVWorkspaceFindsTheMembership(t *testing.T) {
	service := t.TempDir()
	writeFile(t, filepath.Join(service, "module.codefly.yaml"), "name: mod\n")
	uvWorkspaceService(t, service, workspaceRequiresPython)
	writeFile(t, filepath.Join(service, "uv.lock"), "version = 1\n")

	membership, err := enclosingUVWorkspace(service, filepath.Join(service, "code"))
	require.NoError(t, err)
	require.Equal(t, &uvWorkspaceMembership{Root: ".", Member: "code"}, membership)
}

func TestEnclosingUVWorkspaceLeavesAProjectWithItsOwnLock(t *testing.T) {
	service := t.TempDir()
	uvWorkspaceService(t, service, workspaceRequiresPython)
	writeFile(t, filepath.Join(service, "uv.lock"), "version = 1\n")
	writeFile(t, filepath.Join(service, "code", "uv.lock"), "version = 1\n")

	membership, err := enclosingUVWorkspace(service, filepath.Join(service, "code"))
	require.NoError(t, err)
	require.Nil(t, membership, "a project with its own lock is its own root")
}

func TestEnclosingUVWorkspaceHonoursMembersAndExclude(t *testing.T) {
	workspace := &uvWorkspaceTable{Members: []string{"code", "packages/*"}, Exclude: []string{"packages/scratch"}}
	require.True(t, workspace.lists("code"))
	require.True(t, workspace.lists("packages/greeting"))
	require.False(t, workspace.lists("packages/scratch"), "an excluded directory is not a member")
	require.False(t, workspace.lists("tools"), "an unlisted directory is not a member")

	service := t.TempDir()
	writeFile(t, filepath.Join(service, "pyproject.toml"), "[tool.uv.workspace]\nmembers = [\"packages/*\"]\n")
	writeFile(t, filepath.Join(service, "uv.lock"), "version = 1\n")
	writeFile(t, filepath.Join(service, "code", "pyproject.toml"), "[project]\nname = \"x\"\nversion = \"0\"\n")
	membership, err := enclosingUVWorkspace(service, filepath.Join(service, "code"))
	require.NoError(t, err)
	require.Nil(t, membership, "uv treats an unlisted project as standalone")
}

func TestEnclosingUVWorkspaceRefusesAWorkspaceWithoutALock(t *testing.T) {
	service := t.TempDir()
	uvWorkspaceService(t, service, workspaceRequiresPython)
	_, err := enclosingUVWorkspace(service, filepath.Join(service, "code"))
	require.ErrorContains(t, err, "has no uv.lock")
}

func TestEnclosingUVWorkspaceRefusesARootOutsideTheService(t *testing.T) {
	repository := t.TempDir()
	writeFile(t, filepath.Join(repository, "module.codefly.yaml"), "name: mod\n")
	writeFile(t, filepath.Join(repository, "pyproject.toml"), "[tool.uv.workspace]\nmembers = [\"services/*/code\"]\n")
	writeFile(t, filepath.Join(repository, "uv.lock"), "version = 1\n")
	service := filepath.Join(repository, "services", "api")
	writeFile(t, filepath.Join(service, "code", "pyproject.toml"), "[project]\nname = \"api\"\nversion = \"0\"\n")

	_, err := enclosingUVWorkspace(service, filepath.Join(service, "code"))
	require.ErrorContains(t, err, "outside the service")
}

// TestWorkspaceMemberRecipeCopiesTheWorkspace pins the recipe a workspace member
// gets: the workspace copied from its root, the project synced from its own
// directory, the developer's virtual environments and caches ignored, and no
// reference to a `code/uv.lock` that does not exist.
func TestWorkspaceMemberRecipeCopiesTheWorkspace(t *testing.T) {
	builder, _ := createdBuilder(t)
	uvWorkspaceService(t, builder.Location, workspaceRequiresPython)
	if !lockWorkspace(t, builder.Location) {
		t.Skip("uv is not installed: the workspace cannot be locked")
	}
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))
	require.Equal(t, builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_EMITTED, plan.GetScope(),
		"the service directory is the context: the recipe assembles nothing")

	dockerfile := dockerfileOf(t, outputDir, plan)
	require.Contains(t, dockerfile, "COPY . .")
	require.Contains(t, dockerfile, "WORKDIR /app/code")
	require.NotContains(t, dockerfile, "code/uv.lock")
	require.Less(t, strings.Index(dockerfile, "WORKDIR /app/code"), strings.Index(dockerfile, "uv sync --frozen"))
	require.Contains(t, dockerfile, "--no-editable")

	ignore, err := os.ReadFile(filepath.Join(outputDir, plan.GetRecipes()[0].GetDockerignore()))
	require.NoError(t, err)
	require.Contains(t, string(ignore), "**/.venv")
}

// TestAStandaloneProjectRecipeIsUnchangedInShape keeps the default recipe what
// it was for a project that owns its lock: its two files copied, not the tree.
func TestAStandaloneProjectRecipeIsUnchangedInShape(t *testing.T) {
	builder, _ := createdBuilder(t)
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	dockerfile := dockerfileOf(t, outputDir, plan)
	require.Contains(t, dockerfile, "COPY code/pyproject.toml code/uv.lock ./")
	require.NotContains(t, dockerfile, "COPY . .")
}

// TestWorkspaceMemberBuildsTheImage is the claim end to end: a member project
// whose lock is at the workspace root, and whose requires-python the base
// image's Python does not satisfy, builds, and its image imports the workspace
// library with the interpreter the project asked for.
func TestWorkspaceMemberBuildsTheImage(t *testing.T) {
	for _, tool := range []string{"docker", "uv"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: the workspace image cannot be built", tool)
		}
	}
	builder, _ := createdBuilder(t)
	uvWorkspaceService(t, builder.Location, workspaceRequiresPython)
	require.True(t, lockWorkspace(t, builder.Location))
	// A developer's environment in the tree must not reach the image.
	writeFile(t, filepath.Join(builder.Location, ".venv", "pyvenv.cfg"), "home = /nowhere\n")

	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	recipe := plan.GetRecipes()[0]
	tag := fmt.Sprintf("codefly-fastapi-uv-workspace-test:%d", time.Now().UnixMilli())
	build := exec.CommandContext(t.Context(), "docker", "build",
		"-f", filepath.Join(outputDir, recipe.GetDockerfile()),
		"-t", tag, builder.Location)
	// The CLI applies the recipe's ignore file to the context; plain docker
	// reads <Dockerfile>.dockerignore beside the Dockerfile.
	ignore, err := os.ReadFile(filepath.Join(outputDir, recipe.GetDockerignore()))
	require.NoError(t, err)
	writeFile(t, filepath.Join(outputDir, recipe.GetDockerfile()+".dockerignore"), string(ignore))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the workspace member did not build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	out, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", tag,
		"python", "-c", "import sys, greeting; print(sys.version_info[:2], greeting.TEXT)").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "(3, 11)", "the image runs the interpreter the project requires")
	require.Contains(t, string(out), "hello from a workspace member", "the image carries the workspace library")
}
