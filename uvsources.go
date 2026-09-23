package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// The recipe emitted for a service whose pyproject declares a
// `[tool.uv.sources]` path outside its own directory has to assemble its own
// build context. uv resolves such a path against the directory of the
// pyproject.toml that declares it, and it records the same relative path in
// uv.lock. The default context is the service directory and nothing beside it,
// so a path leaving the service is absent from the builder stage and
// `uv sync --frozen` fails there with
//
//	error: Could not compute absolute path from workspace root and lockfile path
//
// while the identical resolution succeeds on the developer's machine. Two
// services sharing one Python project in a repository is a layout this recipe
// has to build; it is not something a consumer should restructure around.
//
// The context is assembled so that every relative path resolves in the builder
// stage exactly as it resolves on the host: the service's own tree and each
// escaping target are placed at their repository-relative paths under one
// root, and the builder stage's working directory is the project's own
// repository-relative path under that root. Nothing in pyproject.toml or
// uv.lock is rewritten — a rewrite would have to keep two files agreeing about
// a path uv reads from both, and a lockfile is a resolver's output, not an
// input a recipe gets to edit.
//
// Containment is the rule: an escaping path must resolve inside the repository
// that owns the service. A path pointing outside it names a directory true on
// one machine only, and is reported as an error naming the declaration rather
// than skipped, because a silently dropped source would build the image against
// a different dependency than the developer builds against.

const (
	// assembledContextDirectory holds every tree the recipe assembles, under
	// the caller-owned output directory. It is output, not source: each
	// emission replaces it wholesale, so a tree carried for a declaration that
	// has since been removed cannot outlive it.
	assembledContextDirectory = "context"

	// builderSourceRoot is where the assembled context is placed in the builder
	// stage. Mirroring the repository-relative layout under it is what makes a
	// relative path resolve to the same directory it resolves to on the host,
	// with no depth left to chance — resolving through the filesystem root
	// would clamp two different paths onto one directory.
	builderSourceRoot = "/src"

	// defaultProjectWorkdir is the builder-stage working directory when no
	// context is assembled: the recipe's context is the service directory and
	// the project is copied straight to it, exactly as before.
	defaultProjectWorkdir = "/app"
)

// repositoryMarkers name the root of the repository that owns a service. A
// codefly module ships as one package from one repository, so its manifest
// marks the same boundary a checkout does — and it still marks it after the CLI
// has resolved the module into its cache, where there is no checkout metadata.
var repositoryMarkers = []string{".git", "module.codefly.yaml", "workspace.codefly.yaml"}

// ephemeralSourceDirectories are Python build outputs and tool caches. They are
// never inputs to an image build, and copying them would make the recipe's
// digest a function of whatever the developer last ran locally — a virtual
// environment also carries absolute paths and symlinks the recipe inventory
// rejects outright.
var ephemeralSourceDirectories = map[string]bool{
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	".tox":          true,
	".git":          true,
	".codefly":      true,
	"node_modules":  true,
}

// CarriedSource is one COPY the builder stage performs for a tree the recipe
// carried: Context is the path inside the emitted build context, Image the
// absolute path the builder stage places it at.
type CarriedSource struct {
	Context string
	Image   string
}

// assembledContext describes a build context this recipe assembled, and is nil
// when the service declares no path source that leaves its own directory.
type assembledContext struct {
	// ServicePrefix is the context-relative directory holding the service's own
	// tree, with a trailing separator. It is empty when no context is
	// assembled, which renders the recipe's COPY paths exactly as before.
	ServicePrefix string
	// ProjectWorkdir is the builder stage's working directory for the project.
	ProjectWorkdir string
	// Carried are the trees the builder stage must place beside the project.
	Carried []CarriedSource
}

// pathSource is one `[tool.uv.sources]` entry that names a directory.
type pathSource struct {
	name     string
	declared string
	resolved string
}

// declaration renders a path source the way pyproject.toml spells it, so an
// error names the line the reader has to change.
func (p pathSource) declaration() string {
	return fmt.Sprintf("pyproject.toml `[tool.uv.sources] %s = { path = %q }`", p.name, p.declared)
}

// assembleUVContext makes the recipe's build context self-contained for a
// service whose project declares a `[tool.uv.sources]` path outside its own
// directory.
//
// serviceDir is the service root (the directory holding service.codefly.yaml),
// sourceDir the Python project inside it, and outputDir the caller-owned recipe
// directory the assembled tree is written to. It returns nil when nothing
// escapes: the recipe then emits only its Dockerfile and dockerignore, claims
// only those, and builds the service directory exactly as before.
func assembleUVContext(serviceDir, sourceDir, outputDir string) (*assembledContext, error) {
	escaping, err := escapingPathSources(sourceDir)
	if err != nil {
		return nil, err
	}
	if len(escaping) == 0 {
		return nil, nil
	}

	serviceRoot, err := filepath.EvalSymlinks(serviceDir)
	if err != nil {
		return nil, err
	}
	repository, err := repositoryRoot(serviceRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, escaping[0].declaration())
	}
	serviceRelative, err := filepath.Rel(repository, serviceRoot)
	if err != nil {
		return nil, err
	}

	// The recipe directory is reused from build to build and the copy only ever
	// adds to it, so a tree an earlier emission left behind would outlive the
	// declaration that asked for it.
	root := filepath.Join(outputDir, assembledContextDirectory)
	if err = os.RemoveAll(root); err != nil {
		return nil, err
	}
	serviceContext := filepath.Join(root, serviceRelative)
	if err = copySourceTree(serviceRoot, serviceContext, outputDir); err != nil {
		return nil, err
	}

	context := &assembledContext{
		ServicePrefix:  assembledContextDirectory + "/" + filepath.ToSlash(serviceRelative) + "/",
		ProjectWorkdir: builderSourceRoot + "/" + filepath.ToSlash(filepath.Join(serviceRelative, filepath.Base(sourceDir))),
	}

	placed := map[string]bool{}
	for _, source := range escaping {
		if within(source.resolved, serviceRoot) {
			return nil, fmt.Errorf("%s resolves to %s, which contains the service itself: the image build context cannot include it",
				source.declaration(), source.resolved)
		}
		if !within(repository, source.resolved) {
			return nil, fmt.Errorf("%s resolves to %s, outside the repository %s that owns the service: the image build context cannot include it",
				source.declaration(), source.resolved, repository)
		}
		relative, relErr := filepath.Rel(repository, source.resolved)
		if relErr != nil {
			return nil, relErr
		}
		slashed := filepath.ToSlash(relative)
		if placed[slashed] {
			// Two declarations may name one directory; carrying it once is enough.
			continue
		}
		placed[slashed] = true
		destination := filepath.Join(root, relative)
		// A target inside the service was copied with it; one outside still has
		// to be carried. Either way the builder stage places it explicitly,
		// because it copies the project's manifests rather than the whole tree.
		if !within(serviceRoot, source.resolved) {
			if err = copySourceTree(source.resolved, destination, outputDir); err != nil {
				return nil, fmt.Errorf("carry %s into the build context: %w", source.declaration(), err)
			}
		}
		context.Carried = append(context.Carried, CarriedSource{
			Context: assembledContextDirectory + "/" + slashed,
			Image:   builderSourceRoot + "/" + slashed,
		})
	}
	return context, nil
}

// escapingPathSources reads the project's pyproject.toml and returns every
// `[tool.uv.sources]` entry naming a directory outside the project, sorted by
// dependency name so an emission is deterministic.
//
// Only `path` entries are resolved. A `[tool.uv.workspace]` member is relative
// to the project too, and one inside the service resolves in the assembled
// context because the whole service tree is placed there; a member naming a
// directory outside the service is not handled here.
func escapingPathSources(sourceDir string) ([]pathSource, error) {
	manifest := filepath.Join(sourceDir, "pyproject.toml")
	content, err := os.ReadFile(manifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var project struct {
		Tool struct {
			UV struct {
				Sources map[string]any `toml:"sources"`
			} `toml:"uv"`
		} `toml:"tool"`
	}
	if err = toml.Unmarshal(content, &project); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifest, err)
	}

	projectRoot, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return nil, err
	}
	var escaping []pathSource
	for name, declared := range project.Tool.UV.Sources {
		for _, entry := range sourceEntries(declared) {
			declaredPath, ok := entry["path"].(string)
			if !ok || declaredPath == "" {
				// A source resolved from an index, a version control system or
				// the workspace carries no directory of its own.
				continue
			}
			target := declaredPath
			if !filepath.IsAbs(target) {
				target = filepath.Join(projectRoot, filepath.FromSlash(target))
			}
			resolved, resolveErr := filepath.EvalSymlinks(target)
			if resolveErr != nil {
				return nil, fmt.Errorf("pyproject.toml `[tool.uv.sources] %s = { path = %q }` resolves to %s, which does not exist",
					name, declaredPath, target)
			}
			if within(projectRoot, resolved) {
				// Inside the project: the default context already carries it.
				continue
			}
			escaping = append(escaping, pathSource{name: name, declared: declaredPath, resolved: resolved})
		}
	}
	sort.Slice(escaping, func(i, j int) bool {
		if escaping[i].name != escaping[j].name {
			return escaping[i].name < escaping[j].name
		}
		return escaping[i].declared < escaping[j].declared
	})
	return escaping, nil
}

// sourceEntries normalizes a `[tool.uv.sources]` value, which uv accepts either
// as one table or as a list of tables carrying environment markers.
func sourceEntries(declared any) []map[string]any {
	switch value := declared.(type) {
	case map[string]any:
		return []map[string]any{value}
	case []any:
		var entries []map[string]any
		for _, element := range value {
			if entry, ok := element.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
		return entries
	default:
		return nil
	}
}

// repositoryRoot walks up from a service's directory to the repository that
// owns it. The walk stops at the first marker rather than the outermost one, so
// a module nested in a workspace of checkouts is bounded by its own checkout.
func repositoryRoot(serviceRoot string) (string, error) {
	for directory := serviceRoot; ; {
		for _, marker := range repositoryMarkers {
			if _, err := os.Lstat(filepath.Join(directory, marker)); err == nil {
				return directory, nil
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("no repository owns the service at %s, so a path source cannot be contained", serviceRoot)
		}
		directory = parent
	}
}

// copySourceTree copies the tree at src into dst, preserving file modes.
//
// Symlinks and other irregular files are skipped: the recipe inventory rejects
// symlinks outright, so a copied symlink would fail plan generation. Build
// outputs and tool caches are skipped for the same reason a lockfile is not
// regenerated here — they are local state, not build inputs.
//
// output is the recipe directory the walk must never descend into: it sits
// inside the service directory in the committed layout, so the walk would
// otherwise copy the growing recipe into itself. Directory identity, not a path
// prefix, decides.
func copySourceTree(src, dst, output string) error {
	outputInfo, err := os.Stat(output)
	if err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			if os.SameFile(info, outputInfo) {
				return filepath.SkipDir
			}
			if p != src && ephemeralSourceDirectories[entry.Name()] {
				return filepath.SkipDir
			}
		}
		relative, relErr := filepath.Rel(src, p)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// within reports whether path is root or sits under it.
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || filepath.IsLocal(relative)
}
