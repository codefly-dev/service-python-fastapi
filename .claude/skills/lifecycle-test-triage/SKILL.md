---
name: lifecycle-test-triage
description: Triage a red or hanging runner-lifecycle test in service-python-fastapi (TestCreateToRunDocker, TestPythonFastAPILifecycle_Matrix, the uvicorn and SBOM tests) to tell a genuine regression from Docker-daemon contention, a missing tool that turned the test into a silent skip, or the default go test timeout. Use before concluding that a lifecycle failure came from your diff, and before raising a timeout or relaxing a wait.
---

# Triaging a lifecycle test

These tests drive a real uvicorn through `uv` and real containers through the
Docker daemon. That makes them the ones worth trusting and the ones that fail
for reasons outside the diff. Work down this list before changing test code.

## 1. Did it actually run?

A missing tool skips, it does not fail, so a green run can mean almost nothing
was exercised:

```bash
go test -v ./... 2>&1 | grep -E '^\s*--- SKIP'
```

| Missing | Skips |
| --- | --- |
| `uv` | the real uvicorn lifecycle |
| `docker` | destroy-under-docker |
| `syft`, `docker`, `uv` | the image SBOM inventory |
| `python3` | the active-environment test |

CI installs `uv` and pins `syft` v1.48.0. If a skip covers the surface you
changed, install the tool and re-run — do not report the green.

## 2. `panic: test timed out after 10m0s`

That is `go test`'s default, not an assertion. Read the `running tests:` block
the panic prints: when it names `TestPythonFastAPILifecycle_Matrix/docker` or
another container case, the suite was still making progress. CI finishes the
whole suite inside the default; a busy local daemon does not.

```bash
go test -timeout 40m ./...
```

If it passes with the longer budget, the timeout was the environment. Say so
rather than raising the timeout in the repo.

## 3. `main_test.go: too many tries`

`TestCreateToRunDocker` polls `GET <address>/version` once a second and gives
up after ten tries. That budget is fixed and small — about eleven seconds —
while the surrounding work (image pull, container create, `uv sync`) is not
bounded by it at all. So this message means *the service did not answer within
eleven seconds of `Start` returning*, which a contended daemon produces on
completely healthy code.

Check contention before suspecting the diff, then re-run the one test:

```bash
docker ps --format '{{.Names}}\t{{.Status}}'
go test -run TestCreateToRunDocker -count=1 -timeout 20m -v ./...
```

Other lazybox sessions share this daemon. A pass in isolation is the verdict.

Do **not** "fix" it by widening the retry budget. The poll loop is asserting
that a started service becomes reachable promptly; a longer wait hides a real
startup regression the next time there is one.

## 4. When it is real

Prove it the way the rule requires — put the suspected cause back and confirm
it breaks. Two failures are almost never environmental:

- **Anything about locking.** `runtime.go` serializes Init/Start/Stop/Destroy
  with a documented lock order (`initMu` → `runnerCreateMu` → `runnerMu`). A
  lost lock is a data race, not a failed assertion, so plain `go test` stays
  green. `go test -race ./...` is the verdict, and it is its own CI job.
- **A status carried in the response, not the error.** `Init` reports failure
  through `InitStatus`, so a `require.NoError` alone passes over a failed Init
  and surfaces later as an unrelated empty-network-mappings error. Read the
  status message in the failure output before tracing the downstream symptom.
