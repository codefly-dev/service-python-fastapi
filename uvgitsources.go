package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/pelletier/go-toml/v2"
)

// A git dependency that needs a credential (a private repository) resolves on
// the developer's machine, whose git has the credential, and fails in the
// builder stage, which has none. The recipe therefore declares every git
// commit the lock pins (DockerBuildRecipe.git_source_downloads): the caller
// fetches each on its own machine before the build and supplies them as the
// uvsourcesContext build context, one bare mirror per repository, and the
// builder stage points git at those mirrors. uv.lock is read, never rewritten.
//
// uv records a git source as `source = { git = "<url>?<query>#<commit>" }` and
// asks git for the commit from <url> by the refspec `+<commit>:refs/commit/<commit>`,
// where <url> is the recorded string up to its query, byte for byte: uv has
// already lowercased the scheme and host and dropped a `git+` prefix when it
// wrote the lock, and keeps a `.git` suffix, a trailing slash, a port and an
// ssh login as they are. git's `url.<mirror>.insteadOf=<url>` matches that
// string by prefix, so the declaration carries it exactly; a normalized form
// would miss the mirror and silently go to the network.

// uvSourcesContext is the build context the Dockerfile reads the caller's git
// mirrors from (the `uvsources` stage in the template).
const uvSourcesContext = "uvsources"

// uvGitMirror is one repository the builder stage maps onto its mirror.
type uvGitMirror struct {
	// Path is the mirror's directory inside the uvsources context.
	Path string
	// Repository is the URL as uv hands it to git.
	Repository string
}

// unmirroredSource is a locked git source the recipe does not declare.
type unmirroredSource struct {
	Package string
	// Reason is core's, or the prefix capture; neither quotes the URL.
	Reason string
}

// uvLockPath is the lock uv syncs the project from: the workspace root's for a
// member of a workspace in the service, the project's own otherwise.
func uvLockPath(serviceDir, sourceDir string, membership *uvWorkspaceMembership) string {
	if membership != nil {
		return filepath.Join(serviceDir, filepath.FromSlash(membership.Root), "uv.lock")
	}
	return filepath.Join(sourceDir, "uv.lock")
}

// lockedGitSources returns the git commits the lock at lockPath pins, sorted
// by repository and commit, each (repository, commit) once: packages built
// from subdirectories of one repository at one commit share a declaration. A
// project with no lock declares nothing; its build fails on the missing lock as
// it always has.
//
// A source core would refuse to declare (services.GitSourceRepositoryProblem:
// http, file, a credential, percent-encoding, "@" in the path, ...) is
// returned as unmirrored, with core's reason, and is neither declared nor
// rendered: the build fetches it itself, as it always has. So is a source
// whose URL begins another locked source's undeclared URL, because git's
// insteadOf rewrites by prefix and would send that source to this mirror. A
// source whose commit is missing or not a full object id makes the lock
// malformed.
func lockedGitSources(lockPath string) (downloads []*builderv0.GitSourceDownload, unmirrored []unmirroredSource, err error) {
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var lock struct {
		Package []struct {
			Name   string `toml:"name"`
			Source struct {
				Git string `toml:"git"`
			} `toml:"source"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(content, &lock); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", lockPath, err)
	}
	type accepted struct{ name, repository, commit string }
	var candidates []accepted
	var locked []string
	for _, pkg := range lock.Package {
		if pkg.Source.Git == "" {
			continue
		}
		// The URL is not quoted in errors: a lock written from a source that
		// named a credential may still carry one.
		source, commit, found := strings.Cut(pkg.Source.Git, "#")
		if !found || !services.IsGitSourceCommit(commit) {
			return nil, nil, fmt.Errorf("%s: package %q is locked to a git source without a full commit id: run `uv lock`", lockPath, pkg.Name)
		}
		repository, _, _ := strings.Cut(source, "?")
		locked = append(locked, repository)
		if reason := services.GitSourceRepositoryProblem(repository); reason != "" {
			unmirrored = append(unmirrored, unmirroredSource{Package: pkg.Name, Reason: "repository " + reason})
			continue
		}
		candidates = append(candidates, accepted{pkg.Name, repository, commit})
	}
	// Leave out every declarable URL that begins an undeclared one, until none
	// does: leaving one out can make it the undeclared URL a shorter one begins.
	for changed := true; changed; {
		changed = false
		declared := map[string]bool{}
		for _, candidate := range candidates {
			declared[candidate.repository] = true
		}
		kept := candidates[:0]
		for _, candidate := range candidates {
			captures := false
			for _, other := range locked {
				if !declared[other] && strings.HasPrefix(other, candidate.repository) {
					captures = true
					break
				}
			}
			if captures {
				unmirrored = append(unmirrored, unmirroredSource{Package: candidate.name, Reason: prefixCaptureReason})
				changed = true
				continue
			}
			kept = append(kept, candidate)
		}
		candidates = kept
	}
	seen := map[[2]string]struct{}{}
	for _, candidate := range candidates {
		key := [2]string{candidate.repository, candidate.commit}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		downloads = append(downloads, &builderv0.GitSourceDownload{Repository: candidate.repository, Commit: candidate.commit, MirrorContext: uvSourcesContext})
	}
	sort.Slice(downloads, func(i, j int) bool {
		if downloads[i].GetRepository() != downloads[j].GetRepository() {
			return downloads[i].GetRepository() < downloads[j].GetRepository()
		}
		return downloads[i].GetCommit() < downloads[j].GetCommit()
	})
	return downloads, unmirrored, nil
}

// uvGitMirrors is each declared repository once, in declaration order, with
// the mirror the caller fetches it into.
func uvGitMirrors(downloads []*builderv0.GitSourceDownload) []uvGitMirror {
	var mirrors []uvGitMirror
	for _, download := range downloads {
		repository := download.GetRepository()
		if len(mirrors) > 0 && mirrors[len(mirrors)-1].Repository == repository {
			continue
		}
		mirrors = append(mirrors, uvGitMirror{Path: services.GitSourceMirrorPath(repository), Repository: repository})
	}
	return mirrors
}

// prefixCaptureReason is why a declarable source is left out when git's
// prefix rewrite for it would also capture an undeclared locked source.
const prefixCaptureReason = "another locked source's URL begins with it, and git's rewrite would capture that source"
