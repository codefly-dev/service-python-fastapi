# Working in `templates/` — what a generated service is

Everything under `templates/` except this file is embedded with `go:embed` and
rendered on `Create`. Editing it changes what every new service receives, and
nothing else: a service already created is never rewritten, bar the two
agent-owned files below. `base/` is not embedded, so a change there reaches
nobody.

| Tree | Rendered when |
| --- | --- |
| `factory/` | always |
| `grpc/` | `grpc-server.enabled` (applied over `factory/`, skipping what exists) |
| `builder/`, `deployment/` | Docker and kustomize recipes, not service source |
| `connect/connect_gen.py` | written verbatim by `Sync`, never rendered |
| `agent/README.md` | the agent's own advertisement |

Two files a generated service holds are in neither tree, because a rendered
template becomes the author's file and these two stay the agent's: the Connect
face (`connect/connect_gen.py`, written by `Sync`) and the ASGI entry
(`hopentry.go`, written on create, sync, update, start and image build).

## Every listener the scaffold emits verifies the hop's admission

A module author must not be able to build a listener that admits traffic the hop
did not stamp, so the scaffold does the verifying and there is **no setting that
turns it off**. One `codefly_hop.Verifier` (`factory/code/src/hop.py`, the
Codefly authority client pinned by commit in
`factory/code/pyproject.toml.tmpl`) is held by `src.hop.ADMISSION` and used by
both listeners:

- the FastAPI app, through `HopAdmission`, a **pure-ASGI** middleware applied by
  the agent's entry around the whole app, so it runs ahead of routing: a path no
  route claims is refused rather than answered, and a route a service author
  adds is admitted without opting in;
- the `grpc.aio` server, through `HopInterceptor`
  (`grpc/code/src/rpc/server.py.tmpl`), whose `serve(port, admission)` has no
  default: a server that cannot verify an admission cannot be started. The
  Connect face relays every protocol onto that server, so there is no second
  Connect-shaped check to keep in step.

### The wrap is the agent's file, not the service's

**Nothing may depend on the author doing auth correctly, and that includes not
deleting a line.** `src/main.py` installs nothing: `codefly_hop_entry.py`
(`hopentry.go`) is what uvicorn and the image's `CMD` serve, it imports
`src.main:app` and wraps it in `HopAdmission` itself, and it is rewritten whole
on every create, sync, update, start and image build. Adding the middleware in
`src/main.py` as well is the one thing to get wrong: an admission is single-use,
so two wraps verify it twice and refuse every call as `stamp_replayed`.
`HopAdmission` passes a scope it already marked straight through for exactly
that reason, so a hand-migrated service that kept its own line still serves.

`Init`, `Build` and `Deploy` read the stack the entry imports — `src/hop.py`
defining `HopAdmission`, `authority-client` in `pyproject.toml` — and warn,
then from `hopStackRequiredFrom` refuse, when it is absent (`hopstack.go`). A
service created before the admission has none of it and migrates by hand:
[docs/hop-migration.md](../docs/hop-migration.md), which is also where the
readiness rewrite `Update` performs is written down.

The method verified is the one the hop signed: `"<VERB> <path>"` for an HTTP
call, as the hop's HTTP face binds it (`libraries/hop/go/forward/http.go` in
codefly-dev/authority), and the full procedure for gRPC and Connect. Getting
that string wrong refuses every genuine call with `stamp_method`.

**Open paths are `/health` and `/ready`, plus the gRPC health service and a
browser's CORS preflight** — none of them is a caller. The list is `OPEN_PATHS`
in `src/hop.py` and `PREFLIGHT_HEADERS` beside it; adding to either publishes a
shape to anyone who can reach the listener. A preflight is open because the hop
signs no admission for one and the app's own CORS layer answers it with headers
alone — which means a route that serves `OPTIONS` itself is the one place a
preflight reaches a handler. A gRPC procedure the server does not serve is
admitted before it is answered, so an unstamped caller learns nothing about
which procedures exist. `/health` and `/ready` are out of the OpenAPI schema
(`include_in_schema=False`) — `openapi/api.swagger.json` is the service's API,
what a dependent service generates a client from and what the hop routes, and a
kubelet's probe is none of that. `/ready` is the declared readiness probe
(`builder.go`), not `/version`: a kubelet carries no admission,
and `/ready` answers 200 only while `Verifier.ready()` holds, so a replica that
cannot admit a call withdraws from rotation instead of refusing every call sent
to it. Never gate readiness on `keys_error`.

### A streaming gRPC handler keeps its own shape

`grpc.aio` accepts two shapes of streaming handler and reads which one it is
driving off the handler it was given: an async generator it iterates, or a
coroutine that writes each answer through the context. `HopInterceptor` selects
its wrapper with `inspect.isasyncgenfunction`, because a wrapper of the wrong
shape fails every call of the other half **after** admission, with a `TypeError`
that reads as a server fault. Both shapes are tested
(`grpc/code/tests/rpc/test_grpc.py.tmpl`).

## The verifier's inputs are delivered, never typed in

`src.hop.build` raises — and the process refuses to start — without:

| Input | Where it comes from |
| --- | --- |
| keys source | `authority/endpoint`, dialled under the authority library's transport rule |
| audience | `CODEFLY__MODULE`, what the authority knows the module by |
| replay store | `hop/replay-connection` (a secret), through `codefly_hop.replaypg` |

The hop's admission key is read from that same `Keys`, never configured. A
developer run (`CODEFLY__ENVIRONMENT=local`, read as the authority library reads
it — not the SDK's `is_local`, codefly-dev/sdk-python#8) relaxes exactly one
thing, and only because the verifier itself relaxes it there: reservations live
in the process (`LocalReservations`) rather than in a store every replica
shares. Nothing else about a local run is different.

## The tests that say so

`factory/code/tests/conftest.py.tmpl` is the authority and its hop as a
generated service's tests run them — a real `Verifier` over a fixture authority,
not a stand-in — and `factory/code/tests/hop/test_admission.py.tmpl` is what
proves the rule: unstamped refused, stamped served, single-use, bound to its own
route, health open, the preflight answered, the wrap in the agent's entry rather
than in `src/`, and the refusal to start. Its client is
`codefly_hop_entry:app`, never `src.main:app` — a suite served the application
without the wrap would pass on a service that admits nothing. They ship to every
service; a change here that drops one of them drops the proof.

This repo's own Go tests boot the scaffold for real, so they compose an
authority for it too: `hop_fixture_test.go`, over
`libraries/hop/go/hoptest` at the same authority commit the scaffold pins.
