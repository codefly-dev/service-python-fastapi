# Native FastAPI ownership qualification

FastAPI owns its uvicorn lifecycle; generic `service-python` supplies shared
Python tooling and retains its own no-op Runtime. Native FastAPI launches use
Core's `NewNativeEnvironment` and `NativeProc`, which owns authenticated process
group registration, birth checks and termination. Core v0.19.10 is the published
dependency; explicit orphan recovery remains Core's protocol consumed by CLI.
No registry, process matcher or recovery implementation belongs in this agent.

Use the maintained `codefly agent deps --pin v0.19.10` operation to adopt the
published module, and `codefly agent build --native-only` to package/install the
agent and adjacent SBOM. Keep the manifest version unchanged for local builds.
Use a disposable OS `HOME` and `CODEFLY_HOME` for those commands; do not use an
incident or shared agent home. The installed binary is local qualification,
not evidence that a version bearing the same label was published or adopted.

The opt-in gate consumes that installed binary and an existing CLI with
`recover process-group --preview/--expect`:

```sh
FASTAPI_NATIVE_AGENT_HOME=/absolute/disposable/codefly-home \
FASTAPI_QUALIFICATION_CLI=/absolute/qualified/codefly \
GOWORK=off go test -count=1 -v -run '^TestNativeAgentOwnershipLifecycle$' .
```

The test creates a separate temporary OS home for registry authentication and
two disposable services. Core's authenticated manager starts each actual agent;
gRPC Load/Init/Start run real uvicorn workloads. Core resolves temporary network
mappings. Each workload reports its own PID and process group; Core authenticates
that group and its exact process birth. Ordinary Stop must remove the first
group and registration while the independent sibling birth and live owner stay
intact. FastAPI Stop releases its environment, so restart uses Init then Start.
At manager load the test captures the authenticated agent-group leader identity.
After restart it requires the current full owner identity to equal that original
birth before Core's identity-bound signal kills only the disposable agent owner.
A stale-birth negative must be refused by that guard, with both original owners
and uvicorn workload births still alive, before the valid owner-death case.
The existing CLI previews that exact authenticated-v2 orphan, then consumes its
unchanged receipt to recover it. The sibling must still retain its exact birth
and live owner, and its ordinary Stop must also remove its registration.

Record source revision/dirty state, module origin and sum, binary and SBOM hashes,
CLI identity, actual test output, and any skipped checks. The test does not prove
Linux operation, sandbox/principal policy, Docker, Nix, full CLI composition,
Wiki `model/model` adoption, or publication. Default tests skip this opt-in gate;
a default green run alone does not qualify an installed agent. Run container and
image-scanning checks only with the fleet's allocated Docker slot.
