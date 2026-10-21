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

const (
	commitA = "8b58bb4786656900153af99dd758fc496b9c3c96"
	commitB = "90db9b742baa9c07532cc08a90d58e790b881312"
)

// gitSourcesLock is a lock in the form uv writes it: two packages built from
// subdirectories of one repository at one commit, the same repository at a
// second commit, an ssh source with a port, sources a recipe cannot declare
// (http, file, a credential, percent-encoding, "@" in the path), and a
// registry package.
var gitSourcesLock = fmt.Sprintf(`version = 1
requires-python = ">=3.12"

[[package]]
name = "liba"
version = "0.1.0"
source = { git = "https://git.example.invalid/acme/mono?subdirectory=liba&rev=%[1]s#%[1]s" }

[[package]]
name = "libb"
version = "0.1.0"
source = { git = "https://git.example.invalid/acme/mono?subdirectory=libb&rev=%[1]s#%[1]s" }

[[package]]
name = "libc"
version = "0.2.0"
source = { git = "https://git.example.invalid/acme/mono?subdirectory=libc&rev=%[2]s#%[2]s" }

[[package]]
name = "libd"
version = "0.1.0"
source = { git = "ssh://git@git.example.invalid:2222/acme/tools.git?rev=main#%[2]s" }

[[package]]
name = "libe"
version = "0.1.0"
source = { git = "http://token@git.example.invalid/acme/plain?rev=main#%[1]s" }

[[package]]
name = "libf"
version = "0.1.0"
source = { git = "file:///srv/local?rev=main#%[1]s" }

[[package]]
name = "libg"
version = "0.1.0"
source = { git = "https://TOKEN@git.example.invalid/acme/secret?rev=main#%[1]s" }

[[package]]
name = "libh"
version = "0.1.0"
source = { git = "https://git.example.invalid/Team%%20Project/repo?rev=main#%[1]s" }

[[package]]
name = "libi"
version = "0.1.0"
source = { git = "https://git.example.invalid/acme/lib@v1?rev=main#%[1]s" }

[[package]]
name = "six"
version = "1.17.0"
source = { registry = "https://pypi.org/simple" }
`, commitA, commitB)

var gitSourcesDeclared = []*builderv0.GitSourceDownload{
	{Repository: "https://git.example.invalid/acme/mono", Commit: commitA, MirrorContext: "uvsources"},
	{Repository: "https://git.example.invalid/acme/mono", Commit: commitB, MirrorContext: "uvsources"},
	{Repository: "ssh://git@git.example.invalid:2222/acme/tools.git", Commit: commitB, MirrorContext: "uvsources"},
}

func requireDeclares(t *testing.T, want, got []*builderv0.GitSourceDownload) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i].String(), got[i].String())
	}
}

// TestLockedGitSourcesDeclaresEachLockedCommitOnce reads the URL uv hands git
// (the recorded one up to its query) and the locked commit, sorted, with a
// repository at one commit declared once however many packages it builds. The
// http and file sources are left out, named by package for the warning, and
// left to the build to fetch as before.
func TestLockedGitSourcesDeclaresEachLockedCommitOnce(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "uv.lock")
	writeFile(t, lock, gitSourcesLock)
	declared, unmirrored, err := lockedGitSources(lock)
	require.NoError(t, err)
	requireDeclares(t, gitSourcesDeclared, declared)
	require.Equal(t, []unmirroredSource{
		{Package: "libe", Reason: "must start with https:// or ssh://"},
		{Package: "libf", Reason: "must start with https:// or ssh://"},
		{Package: "libg", Reason: "must carry no user information over https"},
		{Package: "libh", Reason: "must carry no percent-encoding"},
		{Package: "libi", Reason: "must carry no @ in the path"},
	}, unmirrored)

	mirrors := uvGitMirrors(declared)
	require.Len(t, mirrors, 2, "one mirror per repository")
	require.Equal(t, services.GitSourceMirrorPath("https://git.example.invalid/acme/mono"), mirrors[0].Path)
}

// TestLockedGitSourcesRefusesAnUnpinnedSource: uv always records the full
// commit id, so a git source without one is a malformed lock. The error names
// the package, never the URL, which may carry a credential.
func TestLockedGitSourcesRefusesAnUnpinnedSource(t *testing.T) {
	for _, source := range []string{"https://token@git.example.invalid/acme/mono?rev=main", "https://token@git.example.invalid/acme/mono#8b58bb4"} {
		lock := filepath.Join(t.TempDir(), "uv.lock")
		writeFile(t, lock, fmt.Sprintf("version = 1\n\n[[package]]\nname = \"liba\"\nversion = \"0.1.0\"\nsource = { git = %q }\n", source))
		_, _, err := lockedGitSources(lock)
		require.ErrorContains(t, err, `package "liba"`)
		require.NotContains(t, err.Error(), "token")
	}
}

// TestBuildDeclaresTheLockedGitSources holds the recipe: exactly the lock's git
// sources, the v6 contract, and the mirror mapping in the Dockerfile; without
// git sources, no declaration, the contract and Dockerfile of before.
func TestBuildDeclaresTheLockedGitSources(t *testing.T) {
	builder, _ := createdBuilder(t)
	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	require.Empty(t, plan.GetRecipes()[0].GetGitSourceDownloads())
	require.Equal(t, services.DockerBuildRecipeContractVersion, plan.GetContractVersion())
	requireGolden(t, "no-git-sources", dockerfileOf(t, outputDir, plan))

	writeFile(t, filepath.Join(builder.SourceLocation, "uv.lock"), gitSourcesLock)
	outputDir = t.TempDir()
	plan = emitRecipe(t, builder, outputDir)
	require.NoError(t, services.VerifyDockerBuildPlan(outputDir, plan))
	requireDeclares(t, gitSourcesDeclared, plan.GetRecipes()[0].GetGitSourceDownloads())
	require.Equal(t, services.DockerBuildRecipeGitSourcesContractVersion, plan.GetContractVersion())
	requireGolden(t, "git-sources", dockerfileOf(t, outputDir, plan))
	// What a recipe cannot declare reaches neither the plan nor the Dockerfile.
	for _, rendered := range []string{plan.String(), dockerfileOf(t, outputDir, plan)} {
		for _, refused := range []string{"TOKEN", "Team", "lib@v1", "acme/plain", "/srv/local"} {
			require.NotContains(t, rendered, refused)
		}
	}
}

// TestWorkspaceMemberDeclaresTheRootLocksGitSources reads the lock uv syncs a
// workspace member from: the workspace root's.
func TestWorkspaceMemberDeclaresTheRootLocksGitSources(t *testing.T) {
	builder, _ := createdBuilder(t)
	uvWorkspaceService(t, builder.Location, workspaceRequiresPython)
	writeFile(t, filepath.Join(builder.Location, "uv.lock"), gitSourcesLock)
	plan := emitRecipe(t, builder, t.TempDir())
	requireDeclares(t, gitSourcesDeclared, plan.GetRecipes()[0].GetGitSourceDownloads())
}

// requireGolden compares a rendered Dockerfile, its pinned images replaced, to
// testdata/dockerfile/<name>.golden. The no-git-sources golden is the
// Dockerfile rendered before git sources were declared, byte for byte.
// UPDATE_GOLDEN=1 rewrites them after an intended template change.
func requireGolden(t *testing.T, name, dockerfile string) {
	t.Helper()
	dockerfile = strings.ReplaceAll(dockerfile, runtimeImage.FullName(), "BUILDER_IMAGE")
	dockerfile = strings.ReplaceAll(dockerfile, uvImage, "UV_IMAGE")
	path := filepath.Join("testdata", "dockerfile", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.WriteFile(path, []byte(dockerfile), 0o644))
	}
	golden, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(golden), dockerfile)
}

// run runs a command in dir and fails the test on error.
func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	out, err := command.CombinedOutput()
	require.NoError(t, err, "%s %v: %s", name, args, out)
	return strings.TrimSpace(string(out))
}

// TestLockedGitSourceBuildsFromTheSuppliedMirror is the claim end to end. The
// project depends on a package in a subdirectory of a git repository whose URL
// the build cannot reach, standing in for one it has no credential for; the
// host's uv locks it through a host-side rewrite, as a host credential would
// let it. Built with the mirror context the caller supplies, the image carries
// the package; built without it, the build fails on the unreachable URL.
func TestLockedGitSourceBuildsFromTheSuppliedMirror(t *testing.T) {
	for _, tool := range []string{"docker", "uv", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: the git source image cannot be built", tool)
		}
	}
	const repository = "https://git.example.invalid/example/libraries"
	work := t.TempDir()
	library := filepath.Join(work, "libraries")
	writeFile(t, filepath.Join(library, "lib", "pyproject.toml"), `[project]
name = "example-git-lib"
version = "0.3.0"
requires-python = ">=3.10"

[build-system]
requires = ["uv_build>=0.8,<0.13"]
build-backend = "uv_build"
`)
	writeFile(t, filepath.Join(library, "lib", "src", "example_git_lib", "__init__.py"), "")
	identity := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com"}
	run(t, library, nil, "git", "init", "-q")
	run(t, library, nil, "git", "add", "-A")
	run(t, library, identity, "git", "commit", "-qm", "library")
	commit := run(t, library, nil, "git", "rev-parse", "HEAD")
	served := filepath.Join(work, "served.git")
	run(t, work, nil, "git", "clone", "-q", "--bare", library, served)

	builder, _ := createdBuilder(t)
	writeFile(t, filepath.Join(builder.SourceLocation, "pyproject.toml"), fmt.Sprintf(`[project]
name = "example-service"
version = "0.1.0"
requires-python = ">=3.10"
dependencies = ["example-git-lib"]

[tool.uv]
package = false

[tool.uv.sources]
example-git-lib = { git = %q, subdirectory = "lib", rev = %q }
`, repository, commit))
	rewrite := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url.file://" + served + ".insteadOf", "GIT_CONFIG_VALUE_0=" + repository, "UV_CACHE_DIR=" + filepath.Join(work, "uv-cache")}
	run(t, builder.SourceLocation, rewrite, "uv", "lock", "-q")
	lock, err := os.ReadFile(filepath.Join(builder.SourceLocation, "uv.lock"))
	require.NoError(t, err)
	require.Contains(t, string(lock), fmt.Sprintf("%s?subdirectory=lib&rev=%s#%s", repository, commit, commit))

	outputDir := t.TempDir()
	plan := emitRecipe(t, builder, outputDir)
	recipe := plan.GetRecipes()[0]
	requireDeclares(t, []*builderv0.GitSourceDownload{{Repository: repository, Commit: commit, MirrorContext: "uvsources"}}, recipe.GetGitSourceDownloads())

	// The mirror context as the caller fills it.
	mirrors := filepath.Join(work, "mirrors")
	mirror := filepath.Join(mirrors, services.GitSourceMirrorPath(repository))
	run(t, work, nil, "git", "init", "-q", "--bare", mirror)
	run(t, mirror, nil, "git", "fetch", "-q", served, "+"+commit+":"+services.GitSourceMirrorRef(commit))

	tag := fmt.Sprintf("codefly-fastapi-uv-git-source-test:%d", time.Now().UnixMilli())
	build := func(args ...string) ([]byte, error) {
		args = append([]string{"build", "-f", filepath.Join(outputDir, recipe.GetDockerfile())}, args...)
		return exec.CommandContext(t.Context(), "docker", append(args, builder.Location)...).CombinedOutput()
	}
	out, err := build("-t", tag, "--build-context", "uvsources="+mirrors)
	require.NoError(t, err, "the mirrored git source did not build:\n%s", out)
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	installed, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", tag,
		"python", "-c", "import importlib.metadata as m; print(m.version('example-git-lib'))").CombinedOutput()
	require.NoError(t, err, "%s", installed)
	require.Contains(t, string(installed), "0.3.0", "the image does not carry the git source")

	out, err = build()
	require.Error(t, err, "without the mirror the unreachable URL must fail the build:\n%s", out)
	require.Contains(t, string(out), "git.example.invalid")
}

// TestGitSourceMappingKeepsInheritedGitConfig runs the rendered mapping in the
// builder base image with git configuration already in the environment, as a
// base image or an earlier ENV may set it: the mappings are appended after it,
// and both are in effect.
func TestGitSourceMappingKeepsInheritedGitConfig(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed: the mapping cannot be run")
	}
	builder, _ := createdBuilder(t)
	writeFile(t, filepath.Join(builder.SourceLocation, "uv.lock"), gitSourcesLock)
	outputDir := t.TempDir()
	dockerfile := dockerfileOf(t, outputDir, emitRecipe(t, builder, outputDir))
	start := strings.Index(dockerfile, "RUN --mount=type=bind,from=uvsources,target=/uvsources \\\n")
	end := strings.Index(dockerfile, "mkdir -p /opt/uv/python && uv sync")
	require.True(t, start >= 0 && end > start, "the rendered sync step has no mirror mapping")
	mapping := strings.ReplaceAll(dockerfile[start+len("RUN --mount=type=bind,from=uvsources,target=/uvsources \\\n"):end], "\\\n", "")

	mirrors := t.TempDir()
	writeFile(t, filepath.Join(mirrors, "present"), "")
	mono := "url.file:///uvsources/" + services.GitSourceMirrorPath("https://git.example.invalid/acme/mono") + ".insteadOf"
	out, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--network=none",
		"-e", "GIT_CONFIG_COUNT=1", "-e", "GIT_CONFIG_KEY_0=user.name", "-e", "GIT_CONFIG_VALUE_0=inherited",
		"-v", mirrors+":/uvsources:ro", runtimeImage.FullName(),
		"sh", "-c", mapping+"git config --get user.name && git config --get "+mono+" && echo count=$GIT_CONFIG_COUNT").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Equal(t, "inherited\nhttps://git.example.invalid/acme/mono\ncount=3\n", string(out))
}
