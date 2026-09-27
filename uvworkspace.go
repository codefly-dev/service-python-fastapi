package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// A service's project (`<service>/code`) may be a member of a uv workspace
// rather than a workspace of its own. uv then keeps one lock, and the
// definitions of every member, at the workspace root: `uv sync` run from the
// project discovers the root above it and resolves against that lock, which is
// what the agent's native runner does. A packaged library whose members name
// each other `{ workspace = true }` has to be laid out this way, because uv
// honours that source only under an ancestor root.
//
// The image recipe used to copy `code/pyproject.toml` and `code/uv.lock` and
// nothing else, so such a project, which resolved and ran on the host, failed
// the image build on a lock that is not in `code/`. The recipe now builds it
// the way the host resolves it: the workspace is copied whole and the project
// is synced from its own directory.
//
// The workspace root must be inside the service, which the image build context
// is. A root above it would make the image depend on files the service does
// not own, and is reported rather than built around.

// uvWorkspaceMembership places a project in the uv workspace it belongs to.
type uvWorkspaceMembership struct {
	// Root is the workspace root, relative to the service directory, in slash
	// form ("." when the service directory is the root).
	Root string
	// Member is the project's directory relative to Root, in slash form.
	Member string
}

type uvWorkspaceTable struct {
	Members []string `toml:"members"`
	Exclude []string `toml:"exclude"`
}

// enclosingUVWorkspace returns the uv workspace the project at sourceDir is a
// member of, or nil when the project is its own root: it has its own uv.lock, or
// no workspace above it inside the repository lists it.
func enclosingUVWorkspace(serviceDir, sourceDir string) (*uvWorkspaceMembership, error) {
	project, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(project, "uv.lock")); err == nil {
		return nil, nil
	}
	service, err := filepath.EvalSymlinks(serviceDir)
	if err != nil {
		return nil, err
	}
	// A project outside any repository has no workspace to discover; its own
	// directory is the only candidate, and it has no lock.
	boundary, err := repositoryRoot(service)
	if err != nil {
		boundary = service
	}
	for directory := filepath.Dir(project); within(boundary, directory); directory = filepath.Dir(directory) {
		workspace, found, err := readUVWorkspace(directory)
		if err != nil {
			return nil, err
		}
		if found {
			member, err := filepath.Rel(directory, project)
			if err != nil {
				return nil, err
			}
			if !workspace.lists(filepath.ToSlash(member)) {
				// uv treats a project a workspace does not list as standalone.
				return nil, nil
			}
			if !within(service, directory) {
				return nil, fmt.Errorf("the project %s is a member of the uv workspace rooted at %s, outside the service %s: the image build context cannot include it",
					project, directory, service)
			}
			if _, err := os.Stat(filepath.Join(directory, "uv.lock")); err != nil {
				return nil, fmt.Errorf("the uv workspace rooted at %s, which the project %s is a member of, has no uv.lock: run `uv lock` there", directory, project)
			}
			root, err := filepath.Rel(service, directory)
			if err != nil {
				return nil, err
			}
			return &uvWorkspaceMembership{Root: filepath.ToSlash(root), Member: filepath.ToSlash(member)}, nil
		}
		if directory == boundary {
			break
		}
	}
	return nil, nil
}

// readUVWorkspace reads the `[tool.uv.workspace]` table of directory's
// pyproject.toml, reporting whether the directory is a workspace root.
func readUVWorkspace(directory string) (*uvWorkspaceTable, bool, error) {
	manifest := filepath.Join(directory, "pyproject.toml")
	content, err := os.ReadFile(manifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var document struct {
		Tool struct {
			UV struct {
				Workspace *uvWorkspaceTable `toml:"workspace"`
			} `toml:"uv"`
		} `toml:"tool"`
	}
	if err := toml.Unmarshal(content, &document); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", manifest, err)
	}
	if document.Tool.UV.Workspace == nil {
		return nil, false, nil
	}
	return document.Tool.UV.Workspace, true, nil
}

// lists reports whether the workspace includes member (slash form, relative to
// its root): a `members` glob matches it and no `exclude` glob does. Globs match
// one path at a time, as uv's do for the `*`, `?` and `[...]` forms.
func (workspace *uvWorkspaceTable) lists(member string) bool {
	matches := func(patterns []string) bool {
		for _, pattern := range patterns {
			if ok, _ := path.Match(path.Clean(pattern), member); ok {
				return true
			}
		}
		return false
	}
	return matches(workspace.Members) && !matches(workspace.Exclude)
}

// ephemeralIgnorePatterns are the dockerignore patterns for
// ephemeralSourceDirectories at any depth, in a stable order. A workspace
// recipe copies the service tree whole, and a developer's virtual environment
// or cache in it is never an input to the image.
func ephemeralIgnorePatterns() []string {
	patterns := make([]string, 0, len(ephemeralSourceDirectories))
	for name := range ephemeralSourceDirectories {
		patterns = append(patterns, "**/"+name)
	}
	sort.Strings(patterns)
	return patterns
}

// pathSourcesStayInService checks that every `[tool.uv.sources]` path the
// project or its workspace root declares resolves inside the service. A
// workspace recipe copies the service tree whole, so a path inside it is
// present in the builder stage; one outside it would not be.
func pathSourcesStayInService(serviceDir, sourceDir string, membership *uvWorkspaceMembership) error {
	service, err := filepath.EvalSymlinks(serviceDir)
	if err != nil {
		return err
	}
	for _, declaring := range []string{sourceDir, filepath.Join(service, filepath.FromSlash(membership.Root))} {
		sources, err := escapingPathSources(declaring)
		if err != nil {
			return err
		}
		for _, source := range sources {
			if !within(service, source.resolved) {
				return fmt.Errorf("%s resolves to %s, outside the service %s whose uv workspace the image copies: the image build context cannot include it",
					source.declaration(), source.resolved, service)
			}
		}
	}
	return nil
}
