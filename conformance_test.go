package main

import (
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// transportOwnershipPatterns are the delivery/reconciliation ownership
// concepts a manifest-producing plugin must never carry. A Codefly plugin
// renders deterministic Kubernetes output from normalized inputs; how that
// output is transported, reviewed, or reconciled belongs to a separate
// promotion driver, not here. Each entry is matched case-insensitively.
var transportOwnershipPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)gitops`),
	regexp.MustCompile(`(?i)repourl`),
	regexp.MustCompile(`(?i)targetrevision`),
	regexp.MustCompile(`(?i)appproject`),
	regexp.MustCompile(`(?i)argocd`),
	regexp.MustCompile(`(?i)argoproj`),
	regexp.MustCompile(`(?i)\bflux\b`),
	regexp.MustCompile(`(?i)kubeconfig`),
	regexp.MustCompile(`(?i)reconcil`),
	regexp.MustCompile(`(?i)\bgit\s+(clone|push|commit|checkout|fetch|remote|tag|init|pull)\b`),
	regexp.MustCompile(`(?i)\bpull\s+request\b`),
	regexp.MustCompile(`(?i)kind:\s*Application\b`),
	regexp.MustCompile(`(?i)kind:\s*AppProject\b`),
}

// scan reports every line in content that matches a forbidden pattern,
// formatted as "<name>:<line>: <trimmed source>".
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

// TestRuntimeSourceIsTransportNeutral enforces the manifest-bundle boundary
// on this plugin's runtime source: it must not integrate with Git
// repositories, GitHub/pull requests, Argo CD/Flux reconcilers, kubeconfig,
// or own reconciliation objects (repoURL, targetRevision, Application,
// AppProject). Test files are excluded — this guard itself names the
// forbidden concepts.
func TestRuntimeSourceIsTransportNeutral(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
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
// never reconciliation control-plane objects (Argo Application/AppProject)
// or repository source bindings (repoURL, targetRevision).
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
