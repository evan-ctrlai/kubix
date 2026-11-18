# Independent real-cluster evidence scenarios

These small scenarios exercise failure boundaries separately, without resetting
or repairing the existing `checkout` deployment. They are **controlled fixtures
in a real isolated Kubernetes cluster**, not production incidents or a diagnostic
accuracy benchmark.

## Prerequisites

Prepare the dedicated lab using [the CrashLoop setup](../crashloop/README.md).
Docker, the owned `kubix-lab` kind cluster, Go 1.26+, PowerShell 7.2+ and kubectl
must already be available. The current isolated reader token must authenticate.
The runner verifies cluster/container ownership, isolated kubeconfig integrity
and reader permissions before creating anything. It never changes RBAC or the
user's ordinary kubeconfig.

The Service-denial scenario specifically requires that the existing reader
cannot list EndpointSlices. If permission is already granted, it stops rather
than revoking it or pretending denial was tested. Use another lab/credential
setup only through an explicit operator decision.

## Run one scenario

From the repository root:

```powershell
pwsh ./labs/scenarios/run.ps1 -Scenario stale-current
pwsh ./labs/scenarios/run.ps1 -Scenario healthy-history
pwsh ./labs/scenarios/run.ps1 -Scenario service-mismatch
```

Each command creates its own uniquely named fixture(s) and removes only those
objects in a `finally` block, including when a validation assertion fails.
It verifies kind, namespace, name, nonce label and UID; deletion includes a server
UID precondition so a replacement object cannot be accidentally removed. A lost
create response is reconciled by exact GET and the nonce ownership label. A
cleanup error is reported, not hidden behind a successful validation result.
No bulk deletion is used. Manifests and reports remain under ignored `.local/lab`
for inspection; they contain fixture information, not copied credentials.

| Scenario | Real fixture | Checks |
| --- | --- | --- |
| `stale-current` | Standalone Pod prints explicitly stale historical diagnosis and current failure, then exits 42 without restart | Both lines retained under their actual current-log source; current termination cited; previous-log gap visible; no speculative model diagnosis |
| `healthy-history` | Ready Pod, zero restarts, current log but no previous container instance | Captured healthy state and current log; unavailable previous logs remain unknown history, not invented failure or recovery |
| `service-mismatch` | Ready Pod labelled `app=validation-real`; Service selects `app=validation-wrong` | Exact target mismatch with resolvable source references; denied EndpointSlices remain an explicit gap, never proof of absent endpoints |

The Pod is Ready according to Kubernetes but does not run an HTTP server. The
Service scenario tests **selector/evidence reasoning**, not end-to-end network
connectivity or every possible routing defect. The three scenarios are
independent; a prior scenario's success or resources are not prerequisites.

## Optional configured local model

The default path requires no model server. To verify that the conservative
boundaries remain in effect even with a model configured, start the existing
pinned local model using the CrashLoop lab helpers, then run for example:

```powershell
pwsh ./labs/scenarios/run.ps1 -Scenario stale-current -ModelURL http://127.0.0.1:11436/v1 -Model qwen2.5:3b
```

Only a loopback `/v1` endpoint is accepted. This runner does not start/download
models or use a paid endpoint. It counts actual model requests rather than
assuming that configuring a model means inference occurred. These three
conservative checks expect **zero model requests** and label their result
`deterministic_evidence`. They do not demonstrate improved LLM diagnostic
accuracy. Use `labs/crashloop/lab.ps1 verify -ModelRuntime windows-gpu` separately
for the original ordinary CrashLoop model-summary validation.

## Reports and metrics

Each run writes:

```text
.local/lab/validation-<scenario>-kubix-check-<unique-run>.json
```

The JSON contains UTC start/end, observed investigation milliseconds, actual
model request count, captured/available source counts, rendered citation count,
individual check outcomes, the bounded redacted answer, and limitations. It
omits credentials, host paths, full Kubernetes objects, raw model prompts,
physical addresses and model API keys. Fixture names and synthetic log snippets
remain to make the findings understandable. Review reports before publishing.

- Investigation time includes bounded reads/answer construction, but excludes
  fixture creation, Go build, model startup and cleanup.
- For Pod scenarios, a source is an immutable captured observation. For Service
  scenarios, it is a recorded tool result. These counts are **not directly
  comparable across scenario types**, and neither is an API-operation count.
- Every rendered Pod citation must resolve into its retained capture. Service
  references must resolve into recorded resources, including the exact fixture
  Service and Pod; fixture selector/label values are checked independently.
- A green result covers the named fixture and conservative boundary, not all
  statement semantics, terminal interactions, real-world incident accuracy,
  generalized reliability or an end-to-end benchmark.

Observed historical failures and remediation are retained in
[the November 18 validation record](../crashloop/results/2026-11-18-bounded-validation.md).
Do not replace those failures with a clean-looking success history. Checked-in
results, when present under `results/`, are dated observations, not claims that
later revisions automatically retain those results.

## Offline tests

```powershell
go test ./internal/labtest ./cmd/kubix-scenarios
pwsh ./labs/scenarios/test.ps1
```

These tests make no live Kubernetes/model requests. They cover explicit opt-in,
endpoint/report bounds, fixture independence, privilege restrictions, identity
mismatches, UID-precondition cleanup, source-backed assertions and fabricated
citation rejection. The operator script is also parsed before being declared
ready. The guarded runner is not part of normal application startup.
