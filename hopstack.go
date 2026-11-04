package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What this file answers, and what it cannot.
//
// It answers one question: does this project carry the admission stack at all?
// The hop's admission reached this agent's scaffold in v0.0.127 and nothing
// reached the services created before it — they serve every route to anyone who
// can address the listener, and until this file nothing on any agent path said
// so. A service is not rewritten by its agent, so the installed base migrates
// by hand (docs/hop-migration.md), and what this agent owes it is to say, on
// the paths a service actually runs through, that the admission is not there.
// Until hopStackRequiredFrom that is a warning, because refusing on the day
// this agent is released would stop services that worked yesterday from running
// at all. From that date it is a refusal.
//
// It CANNOT answer "does this service refuse an unstamped call", and nothing
// here should be read as if it does. It reads source text, in files the author
// owns. A src/hop.py whose HopAdmission passes every request through satisfies
// it completely and is then given the full migrated treatment; so does a
// pyproject.toml that names authority-client only in a comment. Those were
// reproduced against this function, not imagined.
//
// The gap closes when HopAdmission, its open set, its preflight test and its
// refusal live in the pinned codefly_hop distribution rather than in the
// service's tree, and this file asks the resolved environment for that
// distribution's version instead of reading the service's source.
// codefly-dev/authority owns that distribution; the PR that added this comment
// states exactly what it has to ship. Until then: this is a detector for a
// service that has no admission, not a gate on one that has a broken one.

// hopStackRequiredFrom is the date the warning becomes a refusal. It is stated
// here, in docs/hop-migration.md and in the warning itself, so a service owner
// reads the same date wherever they meet it.
var hopStackRequiredFrom = time.Date(2027, time.February, 1, 0, 0, 0, 0, time.UTC)

// hopStackNow is time.Now, replaced by the tests that assert both sides of that
// date without waiting for it.
var hopStackNow = time.Now

// What the agent-owned entry imports, and so what a project has to carry for
// the entry to be what uvicorn serves.
const (
	// hopModuleName is src/hop, resolved the way Python resolves it: the module
	// src/hop.py, or the package src/hop/__init__.py. Reading only the former
	// made an ordinary refactor — growing the module into a package — fail
	// Init, Build and Deploy with "is a directory".
	hopModuleName     = "hop"
	hopAdmissionClass = "class HopAdmission"
	// hopDependency is the authority client's distribution name as the
	// scaffold's pyproject declares it. Matched by name alone: the commit it is
	// pinned at moves deliberately, and a project that moved it is migrated.
	hopDependency = "authority-client"
	pyProjectFile = "pyproject.toml"
	mainModule    = "src/main.py"
	// secondWrapCall is the line that must NOT be in src/main.py: the entry is
	// the only wrap.
	secondWrapCall = "add_middleware(HopAdmission)"
)

// hopReadinessPath is the path the admission leaves open for a kubelet, and the
// readiness probe every scaffold declares (builder.go). It answers 200 only
// while the replica holds the authority's published keys.
const hopReadinessPath = "/ready"

// legacyReadinessPath is the readiness path every scaffold declared before the
// hop's admission reached this agent: an ordinary route, which the admission
// now admits like any other.
const legacyReadinessPath = "/version"

// hopStack is what of the admission stack a project carries.
type hopStack struct {
	// module is src/hop defining HopAdmission, which the entry wraps with.
	module bool
	// dependency is the authority client, which src/hop imports the verifier
	// from.
	dependency bool
	// secondWrap is src/main.py installing HopAdmission itself. The entry is
	// the only wrap, and an admission is single-use: a second wrap verifies it
	// twice and refuses every call as stamp_replayed. That is a
	// misconfiguration rather than an installed-base condition, so it is
	// refused on sight rather than dated — and tolerating it, which an earlier
	// version did by letting an already-marked scope through, is what let an
	// application mark every scope from its own lifespan and serve unadmitted.
	secondWrap bool
}

func (stack hopStack) complete() bool { return stack.module && stack.dependency }

// absent names what the project does not carry, in the words a service owner
// has to act on.
func (stack hopStack) absent() string {
	var missing []string
	if !stack.module {
		missing = append(missing, fmt.Sprintf("src/%s.py does not define %s", hopModuleName, hopAdmissionClass))
	}
	if !stack.dependency {
		missing = append(missing, fmt.Sprintf("%s does not require %s", pyProjectFile, hopDependency))
	}
	return strings.Join(missing, ", and ")
}

// readHopStack reads the stack out of the project at sourceDir. A file it
// cannot read is reported, never treated as absent: "the admission is missing"
// and "this agent could not tell" are different answers and only one of them
// is a migration.
func readHopStack(sourceDir string) (hopStack, error) {
	var stack hopStack
	module, err := readPythonModule(filepath.Join(sourceDir, "src"), hopModuleName)
	if err != nil {
		return stack, err
	}
	stack.module = strings.Contains(module, hopAdmissionClass)

	project, err := readIfPresent(filepath.Join(sourceDir, pyProjectFile))
	if err != nil {
		return stack, err
	}
	stack.dependency = strings.Contains(project, hopDependency)

	main, err := readIfPresent(filepath.Join(sourceDir, filepath.FromSlash(mainModule)))
	if err != nil {
		return stack, err
	}
	stack.secondWrap = strings.Contains(main, secondWrapCall)
	return stack, nil
}

// readPythonModule reads the module named name under dir the way an import
// would resolve it: name.py, else name/__init__.py. Absent is "".
func readPythonModule(dir, name string) (string, error) {
	module, err := readIfPresent(filepath.Join(dir, name+".py"))
	if err != nil || module != "" {
		return module, err
	}
	return readIfPresent(filepath.Join(dir, name, "__init__.py"))
}

// readIfPresent is the file's content, or "" when it is not there. Anything
// else — a permission error, a directory where a file belongs — is returned,
// because this agent refusing is not the same as the service lacking something.
func readIfPresent(path string) (string, error) {
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		return string(content), nil
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	default:
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
}

// servesOpenReadiness reports whether this project's own source declares the
// readiness route the admission leaves open.
//
// It gates the readiness rewrite. A halfway migration — src/hop.py copied and
// the dependency added, the /ready route not yet — would otherwise have its
// probe moved onto a path the service answers 404 for, which is the NotReady
// failure the rewrite exists to prevent, caused by the thing preventing it.
// Text again, with the same limits as everything above.
func servesOpenReadiness(sourceDir string) (bool, error) {
	return servesDeclaredRoute(sourceDir, hopReadinessPath)
}

// containsInSource reports whether any .py file under the project's src/ tree
// holds one of needles.
func containsInSource(sourceDir string, needles ...string) (bool, error) {
	found := false
	err := filepath.WalkDir(filepath.Join(sourceDir, "src"), func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case found:
			return fs.SkipAll
		case entry.IsDir() || !strings.HasSuffix(path, ".py"):
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, needle := range needles {
			if strings.Contains(string(content), needle) {
				found = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return found, err
}

// hopStackRefusal is what Init, Start, Build and Deploy answer a project with
// no admission stack, once the window has closed.
func hopStackRefusal(stack hopStack) error {
	return fmt.Errorf("this service serves every route to anyone who can address its listener: %s, "+
		"so no listener it starts verifies the hop's admission. Migrate it (docs/hop-migration.md) — "+
		"since %s this agent refuses to start or deploy a service that cannot admit a call",
		stack.absent(), hopStackRequiredFrom.Format(time.DateOnly))
}

// hopStackWarning is the same thing said while the window is open.
func hopStackWarning(stack hopStack) string {
	return fmt.Sprintf("this service serves every route to anyone who can address its listener: %s, "+
		"so no listener it starts verifies the hop's admission. Migrate it (docs/hop-migration.md); "+
		"from %s this agent refuses to start or deploy it",
		stack.absent(), hopStackRequiredFrom.Format(time.DateOnly))
}

// secondWrapRefusal is what a service that installs the wrap itself is told.
// Undated: this is not an installed-base condition but a service that would
// refuse every call it is given, and saying so beats serving it.
func secondWrapRefusal() error {
	return fmt.Errorf("%s calls %s, and %s applies the wrap already: an admission is single-use, so "+
		"verifying it twice refuses every call as stamp_replayed. Delete that line — the entry is the only wrap",
		mainModule, secondWrapCall, hopEntryModule)
}

// checkHopStack is the one judgement Init, Start, Build and Deploy share: an
// error to refuse with, or a warning to log, or neither. warn is empty when
// there is nothing to say.
func checkHopStack(stack hopStack) (err error, warn string) {
	if stack.secondWrap {
		return secondWrapRefusal(), ""
	}
	if stack.complete() {
		return nil, ""
	}
	if hopStackNow().Before(hopStackRequiredFrom) {
		return nil, hopStackWarning(stack)
	}
	return hopStackRefusal(stack), ""
}

// servedApp is the app uvicorn (and the image's CMD) is pointed at: the
// agent-owned entry when the project carries the stack the entry imports, and
// the application itself when it does not — a project being warned about still
// starts, and pointing it at an entry whose import fails would refuse it with a
// ModuleNotFoundError instead of the sentence above.
func servedApp(stack hopStack) string {
	if stack.complete() {
		return hopEntryApp
	}
	return unadmittedApp
}

// servesDeclaredRoute reports whether any of the project's own Python source
// declares route as a quoted string.
func servesDeclaredRoute(sourceDir, route string) (bool, error) {
	return containsInSource(sourceDir, `"`+route+`"`, `'`+route+`'`)
}
