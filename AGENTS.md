# Working in codefly-dev/service-python-fastapi

This repo is one codefly agent binary: the **FastAPI specialization** of the
generic Python agent. It composes `github.com/codefly-dev/service-python`'s
`pkg/*` types and adds what FastAPI needs — uvicorn supervision and hot reload,
OpenAPI-derived REST endpoints, an opt-in `grpc.aio` listener, the Dockerfile
and kustomize recipes, and the scaffold a new FastAPI service is created from.
Dependency management is `uv`; poetry is not supported.

It does **not** own:

- **Generic Python behaviour** — `uv`, AST/ruff/pytest tooling, the Python
  `Code` and `Tooling` surfaces. Those come from `service-python` and are
  wired through unchanged in `main.go`. A defect there is fixed there.
- **The resource, network and configuration model** — ports, endpoints,
  environment injection, readiness, the agent gRPC contracts. That is
  `codefly-dev/core`.
- **The `codefly` CLI** (`codefly-dev/cli`, private — do not import it, do not
  make CI depend on it) and user workspaces.

When the change you need belongs to one of those, it gets made there.

## How to behave

Fleet standard — [handbook#68](https://github.com/obin-ai/handbook/issues/68).
These bite here because this agent sits between core's resolution machinery and
a user's Python process: nearly every defect is observed as "the service does
not come up", and the local substitute for whatever did not resolve is always
within reach.

- **A gap in the tooling is a bug in the tooling — never a reason to reach
  around it.** When something needs a step `codefly`, `uv` or a companion image
  does not perform, the answer is a capability fixed in whichever tool owns it,
  and named in the PR. Not a hand-assembled substitute — not as a "workaround",
  not "just this once", not "until the capability lands".
- **Never hack. Provide the best fix, even when it spans repos.** The fix
  living in `core` or `service-python` is not a reason to work around it here.
  Open the PR there and consume the reviewed result. When it genuinely cannot
  be fixed now, the deliverable is a precise issue against that owner plus an
  explicitly labelled stopgap — never an unlabelled one.
- **Classify every change that makes something work**, in the PR body: a *fix*
  at the place that owns the behaviour, or a *hack*. A hack does not become a
  fix by working, by being small, by being local, or by the real fix belonging
  elsewhere.
- **Never hardcode what the system resolves.** Ports, endpoint addresses,
  injected environment and credentials are resolved *for* this agent and then
  published by it: `Init` takes the port out of the network mappings, and the
  environment the Python process receives is assembled by core's
  `EnvironmentVariableManager` from endpoints and workspace configurations.
  Typing one of those values in encodes something true only on one machine for
  ten minutes — and it fails *quietly*: a runtime missing a credential can skip
  registration silently, so the service boots, serves, and is simply absent.
- **Diagnose, do not pattern-match.** "It started working when I set X" is not
  a diagnosis — set X back and confirm it breaks. Do not trust an error message
  before checking its claim. `TestCreateToRunDocker` failing with `too many
  tries` is the standing example: it reports a readiness timeout and usually
  means the shared Docker daemon is contended, not that the diff broke startup.
- **Say what you did not verify.** Unverified is not the same as working. The
  Docker-backed and image-scanning tests are the slow, real ones; if you did not
  run them, the PR says so.

## Build and test

Derived from `.github/workflows/ci.yml`, which delegates to core's shared
`go-service-ci.yml` and adds a `race` job of its own. Go comes from `go.mod`
(1.27.0). All of it runs locally:

```bash
go build ./...          # CI: build job
go vet ./...            # CI: build job
go mod tidy -diff       # CI: build job
go test ./...           # CI: test job (runs it with -v)
go test -race ./...     # CI: race job
```

The `race` job exists because the runner lifecycle (Init / Start / Stop /
Destroy) is concurrent gRPC handlers over shared state: a lost lock is a data
race, not a failed assertion, so the plain `go test` above passes on code with
a mutex removed. Touch `runtime.go`'s locking and `-race` is the run that
matters. The lock order is documented on the struct fields — follow it.

**Nothing is mocked.** The suite drives a real uvicorn through `uv`, real
containers through the Docker daemon, and a real `syft` scan of an image built
from the recipe this agent emits. Where a tool is absent most of these tests
skip rather than fake, so a green run proves less than it looks — `uv`, `syft`,
`python3` and `nix` all degrade silently. A missing Docker daemon does not:
`TestCreateToRunDocker` runs in a container runtime context with no tooling
guard, so it fails loudly. Never quiet that failure with a skip guard; a loud
red is the correct outcome and the skip would buy a false green.

**CI has no `nix`, so the nix backend is covered nowhere.**
`testmatrix.ForEachEnvironment` skips any backend missing from the host, and
this repo does not pass `Only(...)` to make absence fatal — so
`TestPythonFastAPILifecycle_Matrix/nix` is the one skip in a green CI run.
Changing `nixflake.go` or `nix/flake.nix` is unverified by CI: exercise it
locally with `nix` installed, and say in the PR that you did.

CI installs `uv` and pins `syft` v1.48.0 in `setup-run` so the other halves do
run. The full skip matrix is in the `lifecycle-test-triage` skill.

The suite is slow because it is real — CI finishes inside `go test`'s default
10-minute timeout, but a contended local Docker daemon pushes the container
cases past it. A local `panic: test timed out` in the lifecycle matrix is that,
not a finding; re-run the suite with `-timeout` raised before concluding
anything from it.

## Where things live

| Path | Owns |
| --- | --- |
| `main.go` | settings, agent advertisement, composition of the generic Python layer |
| `runtime.go` | Init/Start/Stop/Destroy, uvicorn supervision, hot reload, the lock order |
| `builder.go` | `Create` scaffolding, OpenAPI endpoint loading, Docker + kustomize recipes |
| `probes.go`, `probe.py` | endpoint health translated to Kubernetes probes / a local probe |
| `templates/` | **what ships** — embedded via `go:embed`, rendered on `Create` |
| `base/` | a checked-in sample service, embedded nowhere |

`templates/` and `base/` look interchangeable and are not. Only `templates/` is
embedded in the binary, so **editing `base/` changes nothing a user receives**;
it has already drifted from `templates/factory/`. Dependabot watches
`/base/code` for pip updates and `/templates/builder` for the base image, so a
dependency bump landing in `base/code` is not a bump of the scaffold — carry it
into `templates/factory/code/pyproject.toml.tmpl` yourself.

## Rules that bite

- **Endpoint contracts are read from fixed locations in the *generated
  service*** (not paths in this repo): `openapi/api.swagger.json` for REST and
  `proto/api.proto` for gRPC, the latter being core's `standards.ProtoPath`.
  Both Builder and Runtime re-derive endpoints from exactly there, and so does
  any dependent service. The swagger file does not exist at `Create` time — the
  service generates it at runtime from its own `src/openapi.py` — so absent is
  a normal state there, not an error to code around.
- **Readiness is the endpoint's declared health check, never an open port.**
  Probe intents (readiness / startup / liveness) stay separate through
  `resources.PlanEndpointProbes`; collapsing them silently drops a predicate.
- **The runtime image is pinned.** `docker-image` rejects a bare name and
  `:latest`. The default `codeflydev/python` companion is built by core and
  repinned on release — prefer it.
- **A user's `flake.nix` is never overwritten.** The embedded flake is written
  only when the source dir has none.
- **Release needs cgo.** The binary links core's tree-sitter grammars, so
  releases go through the `goreleaser-cross` image; `conformance_test.go` is
  what keeps `.goreleaser.yaml` and the release workflow honest about it.

## Procedures

Step-by-step procedures live in `.claude/skills/`, loaded on demand rather than
carried here:

- `lifecycle-test-triage` — a lifecycle test is red or hanging, and you need to
  tell a real regression from daemon contention, a silent skip, or the timeout.

## Workflow

- Branch and PR; never commit to `main`. Conventional Commits for the title.
- Bumping a dependency: `go get` does not tidy. Run `go mod tidy` afterwards,
  or `ci / build` goes red on a change that passed every other local check.
- `agent.codefly.yaml` carries the agent version; releases are tag-driven
  (`.github/workflows/releaser.yml`), never hand-built.
- Keep this file under ~150 lines (hard cap 200). Push depth into a nested
  `AGENTS.md` beside what it describes, or into `.claude/skills/`.
- There is no `CLAUDE.md`. If one is added, it is a one-line `@AGENTS.md`
  pointer — one canonical source, never a second copy to drift.
- Treat this file as code: the PR that changes a process updates it.
