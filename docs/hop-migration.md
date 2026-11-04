# Migrating a service created before the hop's admission

Every listener the scaffold emits verifies the authority's hop admission. That
reached this agent in v0.0.127; a service created before it has none of it, and
a service is never rewritten by its agent — `Create` renders the scaffold once,
`Update` applies the build and deployment recipes only. So the installed base
migrates by hand, and this is the procedure.

**Until 2027-02-01 the agent warns on `Init`, `Build` and `Deploy` and carries
on.** From that date each of them refuses: a service that cannot admit a call is
not started and not deployed. The date is `hopStackRequiredFrom` in
`hopstack.go`, and a test asserts that this file and the warning state the same
one.

What the agent looks for is the stack its own entry imports — `code/src/hop.py`
defining `HopAdmission`, and `authority-client` in `code/pyproject.toml`. Both
present is a migrated service; the agent then writes `code/codefly_hop_entry.py`
and points uvicorn and the image's `CMD` at it.

## What you are reachable by until you have done this

Every route of the service, by anything that can address the listener. There is
no partial state: the hop in front of the service checks the caller's Work
Context and signs an admission for the call, and a service that does not verify
that admission is one that serves whatever arrives. A deployment is where this
is worst — the listener is up for as long as the Deployment is.

## The migration

Take the three files from a service this agent creates today (create a throwaway
one, or read them in `templates/factory/` in this repo) and keep them as they
are — they are the verification point, not a starting point:

1. `code/src/hop.py` — the verifier, the open paths, the replay store and the
   refusal. Copy it whole.
2. `code/pyproject.toml` — add the `authority-client` requirement, pinned by the
   commit the scaffold pins, and `asyncpg`. Then `uv lock`.
3. `code/src/admin/router.py` — the `/health` and `/ready` routes, both
   `include_in_schema=False`. `/ready` answers 200 only while the replica holds
   the authority's keys.

Do **not** add `app.add_middleware(HopAdmission)` to `code/src/main.py`. The wrap
is `codefly_hop_entry.py`'s, which the agent writes on the next `codefly sync
service` (or `update`, `build`, or run) — that is the point of the move, and a
second wrap in your own source would verify each single-use admission twice and
refuse every call as replayed.

Then `codefly sync service <name>`. The agent writes the entry and moves the
readiness probe in `service.codefly.yaml` off `/version` for you.

## Readiness: why it has to move

A manifest scaffolded before the admission declares readiness on `/version`,
which is an ordinary route. The moment the admission is installed, `/version` is
admitted like every other route, and a kubelet carries no admission: it reads
401, every replica is `NotReady` for ever, and nothing in the rollout says why.
`/ready` is one of the two paths the admission leaves open.

`Update` rewrites that probe — from `/version` to `/ready`, statuses `200` —
for a service that carries the stack, and leaves it alone for one that does not,
because a service with no admission still serves `/version` to a kubelet and
pointing its probe at a route it does not have would take out exactly the
replicas this is here to keep in rotation. If you changed the readiness path to
something of your own, the rewrite does not touch it: point it at a path the
admission leaves open yourself.

## What the composition has to deliver

The process refuses to start without these, which is the same refusal the hop
makes:

| Input | What it is |
| --- | --- |
| `authority/endpoint` | where the authority's published keys are read |
| `CODEFLY__MODULE` | the audience the authority knows the module by |
| `hop/replay-connection` | the Postgres each single-use admission's nonce is reserved in, shared by every replica — not needed in the run named exactly `local` |

The connection needs `CREATE` on its schema the first time any replica of any
such service starts, for the reservation table, or run `replaypg.MIGRATION` once
as a composition step.

## Checking it

A migrated service's own suite proves the rule: copy `code/tests/conftest.py`
and `code/tests/hop/` too, and `uv run pytest`. An unstamped call refused, a
stamped one served, an admission single-use and bound to its own route and
module, health open, and the wrap in the agent's entry rather than in your
source. Without those tests nothing in the service says the rule holds.
