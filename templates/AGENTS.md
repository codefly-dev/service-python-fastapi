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

### Where the wrap is applied is the agent's. What the wrap IS is not, yet.

**Read this before citing the rule as settled.** `src/main.py` installs nothing:
`codefly_hop_entry.py` (`hopentry.go`) is what uvicorn and the image's `CMD`
serve, it imports `src.main:app`, wraps it in `HopAdmission`, and is rewritten
whole on every create, sync, update, start and image build. So the edit that
used to remove the admission — deleting a line from `src/main.py` — no longer
exists, and a route an author adds is admitted without opting in.

But `HopAdmission`, `OPEN_PATHS`, `PREFLIGHT_HEADERS` and the refusal all still
live in `factory/code/src/hop.py`, which is the author's file from `Create` on,
imported into the author's own interpreter. The entry narrows that as far as one
module can: it reads the open set and the preflight test, and takes hold of the
wrapper function itself, **before** any module of the service is imported, so
the two bypasses reproduced against the first version of it — rebinding
`hop.OPEN_PATHS` or `hop.PREFLIGHT_HEADERS` from `src/main.py` — no longer reach
what a request is tested against. Three channels remain open: a no-op
`HopAdmission` in `src/hop.py`, a line that rebinds `src.hop._refuse` or
`Admission.admit`, and any other edit to that module. **`hopstack.go` cannot
detect any of them** — it reads source text, and a `class HopAdmission` that
passes every request through satisfies it completely.

So the honest statement of the rule today is: *the agent installs the admission
for you and an ordinary service cannot lose it by accident*, **not** *an author
cannot take it out*. Closing the difference means `HopAdmission` and its policy
living in the pinned `codefly_hop` distribution rather than in the service tree,
with the gate asking the resolved environment for that distribution's version
instead of grepping source. That distribution is codefly-dev/authority's to
ship; the PR that wrote this paragraph specifies what it has to export.

`Init`, `Start`, `Build` and `Deploy` read the stack the entry imports —
`src/hop` defining `HopAdmission`, `authority-client` in `pyproject.toml` — and
warn, then from `hopStackRequiredFrom` refuse, when it is absent
(`hopstack.go`). That detects a service with **no** admission, which is the
whole installed base, and is what it is for. `Start` re-reads rather than
trusting `Init`, because `Init` answers a response field and `Start` is the call
that opens the socket.

**One wrap, and a second is refused.** `src/main.py` calling
`add_middleware(HopAdmission)` fails `Init` on sight: an admission is
single-use, so two wraps verify it twice and refuse every call as
`stamp_replayed`. An earlier version absorbed it instead, by letting a scope
that was already marked through — which let an application mark every scope from
its own `lifespan` and serve every request unadmitted. Tolerance was the hole.

A service created before the admission migrates by hand:
[docs/hop-migration.md](../docs/hop-migration.md).

The method verified is the one the hop signed: `"<VERB> <path>"` for an HTTP
call, as the hop's HTTP face binds it (`libraries/hop/go/forward/http.go` in
codefly-dev/authority), and the full procedure for gRPC and Connect. Getting
that string wrong refuses every genuine call with `stamp_method`.

**Open paths are `/health` and `/ready`, plus the gRPC health service and a
browser's CORS preflight** — none of them is a caller. `OPEN_PATHS` and
`PREFLIGHT_HEADERS` in `src/hop.py` are the defaults; the entry reads them once
and the wrap tests every request against its own copy, so editing them changes
what a NEW process opens and rebinding them later changes nothing. Adding to
either publishes a shape to anyone who can reach the listener. A preflight is
open because the hop signs no admission for one and the app's own CORS layer
answers it with headers alone — and "a browser's preflight" is two header NAMES,
which any caller can present, so a route that serves `OPTIONS` itself is a
handler reachable unadmitted. The gRPC health exemption is matched by the
**identity** of the registered health servicer, not by a prefix on
`details.method`, which is built from proto names the author declares. A gRPC
procedure the server does not serve is admitted before it is answered, so an
unstamped caller learns nothing about which procedures exist — through a
`stream_stream` handler, because grpc.aio reads a unary request message before
invoking the behavior and a client-streaming caller that sends nothing would
hang unadmitted. `/health` and `/ready` are out of the OpenAPI schema
(`include_in_schema=False`) — `openapi/api.swagger.json` is the service's API,
what a dependent service generates a client from and what the hop routes, and a
kubelet's probe is none of that. `/ready` is the declared readiness probe
(`builder.go`), not `/version`: a kubelet carries no admission,
and `/ready` answers 200 only while `Verifier.ready()` holds, so a replica that
cannot admit a call withdraws from rotation instead of refusing every call sent
to it. Never gate readiness on `keys_error`.

### A refusal has to be readable by a browser

The wrap is outside the app's CORS layer, so a 401 is produced where that layer
cannot add headers to it. Without them a browser gets an opaque `TypeError` from
`fetch` and a front end cannot tell "your stamp expired, re-stamp" from "the
service is down" — which is a working-software break for every browser-facing
module, not a hole. `_refuse` therefore emits them itself: the caller's own
`Origin` echoed, `Access-Control-Allow-Credentials`, `Vary: Origin`. Nothing
private is published by making the refusal readable; its body is the verifier's
fixed code and text. Asserted at `factory/code/tests/hop/test_admission.py.tmpl`.

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
