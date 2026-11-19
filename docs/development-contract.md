# Building and validating a FastAPI change

1. **What a person exercises is a composed service.** A playground is a service
   in this repo's own workspace, started by `codefly run service`, reaching its
   API through a declared dependency and an `${endpoint:…}` reference, with
   identities from `--fixture`. A surface with no backend is a story on
   fixtures. Never a hand-written dev server, a static page beside it, a
   launcher that passes an address between processes, or a demo container. A
   module that cannot be exercised without a host is a fixture gap filed
   against the CLI or the SDK, never a harness written here.
2. **Code generated from a contract comes from `codefly generate`** — `proto`,
   `client`, `contracts`, `runnables`, `runnable-bindings`. Never this repo's
   own generator pipeline, never a hand-written client for a contract that has
   one, never a local gate on hand-generated output. A missing language, plugin
   or flag is an issue against the CLI, named in the pull request.
3. **Never hardcode anything the system resolves.** No address, port, DSN,
   credential or host name is written down — not in code, configuration,
   tests, scripts, docs, or a command you type. Addresses come from
   `codefly.For(ctx)`, from `${endpoint:…}` in configuration, from the
   generated client in a frontend, and from the suite's resolver in a test;
   secrets from `codefly config generate`; a running service's address from
   `codefly endpoint`. A value the SDK or the CLI cannot resolve, or resolves
   wrong, is a bug in that tool to fix, not a value to type or copy from a
   log: typing it encodes something true on one machine for ten minutes, and
   it fails quietly.
4. **Local first, from the lowest level; no pull request until it is
   validated on an environment.** Every change climbs this ladder in order and
   is fixed at the first rung that fails: (1) unit tests; (2) composed tests
   with the Codefly SDK (`WithDependencies`) and the local authority, so
   permissions are proven, not assumed; (3) this repo's playground, exercised
   in a browser; (4) the solution run locally (`codefly run`, the local authority and its fixture people); (5) a deploy to a non-production environment
   (a dev cell) built from the local branch — no CI, no merge, and the
   only push is the gitops promotion; (6) QA and validation there. Only then
   (7) the pull request — even for an approved release. A **product or
   contract change** — a boundary, a capability, a story's promise, a
   decision, a concept, a word, a gate — is still proposed first; a technical
   change is proposed, if a contract has to move at all, only after it runs.
   Integration tests driven from the API against a running environment are the
   next rung, being built.
5. **No cluster tests.** No test — CI job, workflow step, make target, script
   or build-tagged suite — starts k3s, k3d, kind, minikube or any Kubernetes
   cluster. Authorization and admission are proven in-process against the
   authority core; a deployment is validated by `codefly run` and a hosted dev
   cell, never by a test.
