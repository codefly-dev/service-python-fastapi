package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// transportOwnershipPatterns are the delivery/reconciliation ownership
// concepts a manifest-producing plugin must never carry. A Codefly plugin
// renders deterministic Kubernetes output from normalized inputs; how that
// output is transported, reviewed, or reconciled belongs to a separate
// promotion driver, not here. Each entry is matched case-insensitively.
//
// Patterns target the tokens these concepts actually carry in Go source and
// Kubernetes manifests — import paths, API groups, resource kinds, and
// process invocations — not the plain-English word, which real integrations
// never emit. "flux" appears only as "fluxcd" (the module path and the
// *.toolkit.fluxcd.io API groups); Git integration arrives as the go-git
// library or an exec of the git binary, never as the prose "git clone".
var transportOwnershipPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)gitops`),
	regexp.MustCompile(`(?i)repourl`),
	regexp.MustCompile(`(?i)targetrevision`),
	regexp.MustCompile(`(?i)appproject`),
	regexp.MustCompile(`(?i)argocd`),
	regexp.MustCompile(`(?i)argoproj`),
	regexp.MustCompile(`(?i)fluxcd`),
	regexp.MustCompile(`(?i)kubeconfig`),
	regexp.MustCompile(`(?i)reconcil`),
	regexp.MustCompile(`(?i)go-git`),
	regexp.MustCompile(`(?i)exec\.Command(?:Context)?\([^"]*"git"`),
	regexp.MustCompile(`(?i)\bgit\s+(clone|push|commit|checkout|fetch|remote|tag|init|pull)\b`),
	regexp.MustCompile(`(?i)\bpull\s+request\b`),
	regexp.MustCompile(`(?i)kind:\s*Application\b`),
	regexp.MustCompile(`(?i)kind:\s*AppProject\b`),
	regexp.MustCompile(`(?i)kind:\s*GitRepository\b`),
}

// scanTransportOwnership reports every line in content that matches a
// forbidden pattern, formatted as "<name>:<line>: <trimmed source>".
func scanTransportOwnership(name, content string) []string {
	var hits []string
	for i, line := range strings.Split(content, "\n") {
		for _, re := range transportOwnershipPatterns {
			if re.MatchString(line) {
				hits = append(hits, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				break
			}
		}
	}
	return hits
}

// isIgnoredSourceDir reports whether a directory is excluded from a module's
// compiled runtime source. This mirrors the Go toolchain's own rule:
// directories named "testdata" and those beginning with "." or "_" are never
// built.
func isIgnoredSourceDir(name string) bool {
	return name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// runtimeGoFiles returns every compiled runtime Go file under root: all
// non-test .go files, recursively, excluding toolchain-ignored directories.
// Test files are excluded because boundary guards (this file included) name
// the forbidden concepts as data.
func runtimeGoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && isIgnoredSourceDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// TestRuntimeSourceIsTransportNeutral enforces the manifest-bundle boundary
// on this plugin's runtime source: it must not integrate with Git
// repositories, GitHub/pull requests, Argo CD/Flux reconcilers, kubeconfig,
// or own reconciliation objects (repoURL, targetRevision, Application,
// AppProject). The scan is recursive so a future subpackage cannot escape it.
func TestRuntimeSourceIsTransportNeutral(t *testing.T) {
	files, err := runtimeGoFiles(".")
	if err != nil {
		t.Fatalf("enumerate runtime source: %v", err)
	}
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if hits := scanTransportOwnership(name, string(src)); hits != nil {
			t.Errorf("runtime source names a transport/reconciler concept:\n  %s", strings.Join(hits, "\n  "))
		}
	}
}

// TestOwnedManifestsAreTransportNeutral enforces the same boundary on the
// plugin-owned deployment templates: they carry only workload/resources,
// never reconciliation control-plane objects (Argo Application/AppProject,
// Flux GitRepository) or repository source bindings (repoURL, targetRevision).
func TestOwnedManifestsAreTransportNeutral(t *testing.T) {
	err := fs.WalkDir(deploymentFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(deploymentFS, path)
		if err != nil {
			return err
		}
		if hits := scanTransportOwnership(path, string(content)); hits != nil {
			t.Errorf("owned manifest names a transport/reconciler concept:\n  %s", strings.Join(hits, "\n  "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deployment templates: %v", err)
	}
}

// TestScanTransportOwnershipDetectsRealVectors pins the detector to the forms
// transport ownership actually takes in Go source and manifests — library
// imports, API groups, resource kinds, and process invocations — and to a set
// of benign lines that must not trip it.
func TestScanTransportOwnershipDetectsRealVectors(t *testing.T) {
	forbidden := []string{
		`_ "github.com/fluxcd/pkg/apis/meta"`,
		`apiVersion: source.toolkit.fluxcd.io/v1`,
		`kind: GitRepository`,
		`_ "github.com/argoproj/argo-cd/v2/pkg/apis"`,
		`import gogit "github.com/go-git/go-git/v5"`,
		`r, _ := exec.Command("git", "clone", url).Output()`,
		`out, _ := exec.CommandContext(ctx, "git", "push").CombinedOutput()`,
		`kind: Application`,
		`spec: { repoURL: https://example.com, targetRevision: main }`,
		`// reconcile the cluster state`,
	}
	for _, line := range forbidden {
		if hits := scanTransportOwnership("x.go", line); hits == nil {
			t.Errorf("expected forbidden line to be flagged: %q", line)
		}
	}

	benign := []string{
		`import _ "github.com/codefly-dev/core/agents/services"`,
		`s.Wool.Debug("git status is clean")`,
		`kind: Deployment`,
		`kind: Service`,
		`newTag: {{.Image.Tag}}`,
		`// influx of requests handled by the digit parser`,
	}
	for _, line := range benign {
		if hits := scanTransportOwnership("x.go", line); hits != nil {
			t.Errorf("benign line falsely flagged: %q -> %v", line, hits)
		}
	}
}

// TestRuntimeGoFilesRecursesAndFilters proves the enumerator descends into
// subpackages (so no future package escapes the boundary check) while
// excluding test files and toolchain-ignored directories.
func TestRuntimeGoFilesRecursesAndFilters(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("top.go")
	write("sub/pkg/deep.go")
	write("sub/pkg/deep_test.go")
	write("testdata/fixture.go")
	write(".hidden/skip.go")

	files, err := runtimeGoFiles(root)
	if err != nil {
		t.Fatalf("runtimeGoFiles: %v", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		got[filepath.ToSlash(rel)] = true
	}

	want := []string{"top.go", "sub/pkg/deep.go"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("expected %s to be scanned, got %v", w, files)
		}
	}
	excluded := []string{"sub/pkg/deep_test.go", "testdata/fixture.go", ".hidden/skip.go"}
	for _, e := range excluded {
		if got[e] {
			t.Errorf("%s should have been excluded from the scan", e)
		}
	}
}

// releaseConfig is the subset of .goreleaser.yaml this repo asserts on: the
// per-target build environments.
type releaseConfig struct {
	Builds []struct {
		ID  string   `yaml:"id"`
		Env []string `yaml:"env"`
	} `yaml:"builds"`
}

// binaryNeedsCgo reports whether the agent binary can still be built with CGO
// disabled. Core's source inspection links real tree-sitter grammars, whose
// bindings carry `//go:build cgo`; with CGO off they contribute no files and
// the build fails at load time rather than degrading. Loading is enough to
// find that out, so this costs a fraction of a second.
func binaryNeedsCgo(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", os.DevNull, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "build constraints exclude all Go files") {
		t.Fatalf("cgo-free build failed for an unexpected reason: %v\n%s", err, out)
	}
	return err != nil
}

// TestReleaseCanCompileTheCgoTheBinaryNeeds is the guard the release path had
// no way to fail on: `ci.yml` builds with the runner's default (CGO on) and
// the release workflow only runs on a tag, so a dependency bump that pulls in
// tree-sitter turns every release red and nothing says so until a tag is cut.
//
// Two halves, and both are load-bearing. `CGO_ENABLED=0` in the build config
// fails the compile outright. `CGO_ENABLED=1` on a bare runner fails too, just
// later and differently — an ubuntu runner has no macOS compiler — which is
// why the release has to go through the cross toolchain image.
func TestReleaseCanCompileTheCgoTheBinaryNeeds(t *testing.T) {
	if !binaryNeedsCgo(t) {
		t.Skip("the binary builds without cgo, so the release toolchain is unconstrained")
	}

	raw, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	var config releaseConfig
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	if len(config.Builds) == 0 {
		t.Fatal(".goreleaser.yaml declares no builds")
	}
	for _, build := range config.Builds {
		var enabled bool
		for _, env := range build.Env {
			if env == "CGO_ENABLED=1" {
				enabled = true
			}
			if env == "CGO_ENABLED=0" {
				t.Errorf("build %q disables cgo; the binary links tree-sitter and will not compile", build.ID)
			}
		}
		if !enabled {
			t.Errorf("build %q does not set CGO_ENABLED=1; goreleaser-cross defaults are not guaranteed", build.ID)
		}
	}

	workflow, err := os.ReadFile(filepath.Join(".github", "workflows", "releaser.yml"))
	if err != nil {
		t.Fatalf("read releaser.yml: %v", err)
	}
	if !strings.Contains(string(workflow), "goreleaser-cross") {
		t.Error("the release workflow does not run goreleaser through the cross toolchain image, " +
			"so CGO_ENABLED=1 has no macOS or cross-linux compiler to use")
	}
}
