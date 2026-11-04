package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The hop's admission reached this agent's scaffold in v0.0.127. Nothing
// reached the services created before it: they keep serving every route to
// anyone who can address the listener, and until this file nothing on any
// agent path said so. A service is not rewritten by its agent — Create renders
// the scaffold once and Update applies the builder recipes only — so the
// installed base migrates by hand (docs/hop-migration.md), and what this agent
// owes it is to say, on the paths a service actually runs through, that the
// admission is not there.
//
// It says it twice over. Until hopStackRequiredFrom, Init and Deploy warn and
// carry on, because a refusal would stop services that worked yesterday from
// running at all. From that date they refuse: a window to migrate in is a
// migration path, a warning nobody reads for ever is not.

// hopStackRequiredFrom is the date the warning becomes a refusal. It is stated
// here, in docs/hop-migration.md and in the warning itself, so a service owner
// reads the same date wherever they meet it.
var hopStackRequiredFrom = time.Date(2027, time.February, 1, 0, 0, 0, 0, time.UTC)

// hopStackNow is time.Now, replaced by the tests that assert both sides of that
// date without waiting for it.
var hopStackNow = time.Now

// What the agent-owned entry imports, and so what a project has to carry for
// the entry to be what uvicorn serves: the verifier's module and the authority
// client it is in.
const (
	hopModulePath     = "src/hop.py"
	hopAdmissionClass = "class HopAdmission"
	// hopDependency is the authority client's distribution name as the
	// scaffold's pyproject declares it. Matched by name alone: the commit it is
	// pinned at moves deliberately, and a project that moved it is migrated.
	hopDependency = "authority-client"
	pyProjectFile = "pyproject.toml"
)

// hopReadinessPath is the path the admission leaves open for a kubelet, and the
// readiness probe every scaffold declares (builder.go). It answers 200 only
// while the replica holds the authority's published keys.
const hopReadinessPath = "/ready"

// hopStack is what of the admission stack a project carries.
type hopStack struct {
	// module is src/hop.py defining HopAdmission, which the entry wraps with.
	module bool
	// dependency is the authority client, which src/hop.py imports the
	// verifier from.
	dependency bool
}

func (stack hopStack) complete() bool { return stack.module && stack.dependency }

// absent names what the project does not carry, in the words a service owner
// has to act on.
func (stack hopStack) absent() string {
	var missing []string
	if !stack.module {
		missing = append(missing, fmt.Sprintf("%s does not define %s", hopModulePath, hopAdmissionClass))
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
	module, err := os.ReadFile(filepath.Join(sourceDir, filepath.FromSlash(hopModulePath)))
	switch {
	case err == nil:
		stack.module = strings.Contains(string(module), hopAdmissionClass)
	case errors.Is(err, os.ErrNotExist):
	default:
		return stack, fmt.Errorf("cannot read %s: %w", hopModulePath, err)
	}
	project, err := os.ReadFile(filepath.Join(sourceDir, pyProjectFile))
	switch {
	case err == nil:
		stack.dependency = strings.Contains(string(project), hopDependency)
	case errors.Is(err, os.ErrNotExist):
	default:
		return stack, fmt.Errorf("cannot read %s: %w", pyProjectFile, err)
	}
	return stack, nil
}

// hopStackRefusal is what Init and Deploy answer a project with no admission
// stack, once the window has closed.
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
