# Working in `templates/builder/` — the build recipe

`Dockerfile.tmpl` and `dockerignore.tmpl` are the recipe this agent emits. It
never runs `buildx`: the caller builds what the plan claims, so everything the
build depends on has to be *stated here* rather than inherited from whatever
the machine happens to have. The Go side is `builder.go` (`Build`),
`uvsources.go`, `uvgitsources.go`, `uvworkspace.go` and `hopentry.go`.

## The recipe owns its uv

The builder stage copies a pinned uv (`uvVersion` in `main.go`) instead of
running whichever one the base image was built with. Without that, a project
written against a newer `[tool.uv]` key resolves on the developer's machine and
fails inside the builder stage with a TOML parse error naming a field that uv
there does not know.

## The recipe owns its build context

A `[tool.uv.sources]` path leaving the project makes the recipe assemble its own
context, mirroring the repository-relative layout so the same relative path
resolves there. That plan claims the whole tree
(`RECIPE_INVENTORY_SCOPE_TREE`); the Dockerfile-only one claims just what it
emitted. **Never rewrite `pyproject.toml` or `uv.lock` to make a path resolve.**

A project that is a member of a uv workspace rooted in the service is built from
that workspace, copied whole, and synced from its own member directory — which
is how `uv sync` resolves it on the host.

## Git sources are declared, by the URL the lock records

A git source is declared in the recipe (`git_source_downloads`) by the URL
exactly as `uv.lock` records it up to its query, which is what uv hands git; the
caller fetches it with its own credentials and the Dockerfile maps that URL onto
the mirror. A normalized URL would miss it silently. A source core refuses, or
whose URL begins an undeclared locked one (insteadOf is a prefix match), is not
declared, and a warning says why. An image whose own git config already rewrites
a declared URL keeps that rewrite — git prefers it — and fetches that source as
it does without a mirror.

## What the runtime stage serves

`CMD` serves the ASGI entry this agent owns (`hopentry.go`), never
`src.main:app`: the entry is what wraps the app in the hop's admission, so the
image serves exactly what a native start serves. A project that carries no
admission stack gets the application instead, after the warning `Build` emits
(`hopstack.go`). The uvicorn logging configuration and its filter module are
copied the same way, written into the project before the context is assembled.
