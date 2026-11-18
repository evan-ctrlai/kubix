# Kubix focused repair validation — 2026-11-13

## Repair summary

- The Pod `c` action can explicitly select the sole container after opening details. The `i` action now describes a selected-Pod investigation rather than requiring CrashLoopBackOff.
- Selected healthy/non-CrashLoop containers use the existing bounded capture: current logs are sampled, and unavailable previous logs remain an explicit cited gap. The no-model fallback now states that missing prior logs do not prove no earlier failure occurred.
- Pod details expose at most 50 sorted, redacted labels and declared container ports. List evidence shares only the fields needed for routing diagnosis.
- `endpointslices` is a read-only resource kind with bounded projections for Service name, ports, endpoint readiness and Pod target references. Raw endpoint IPs are omitted. A 403 remains an error, never an empty endpoint result. The agent is instructed to compare selectors/ports and retain permission gaps.
- The fresh-lab reader Role manifest grants only get/list on EndpointSlices. The lab preflight treats that read as optional: denial is reported as unavailable endpoint evidence while other read-only browsing remains usable. **No live RBAC or credential change was made.**
- Added a deterministic stale/current fixture using `restartPolicy: Never`; it emits both signals, exits 42, and avoids restart-backoff timing.

## Checks

| Check | Result |
|---|---|
| `go test ./...` | **PASS** — all packages passed; `cmd/kubix` has no test files. Final run included the EndpointSlice read-only action assertion. |
| `pwsh -NoProfile -File ./labs/crashloop/cluster.Tests.ps1` | **PASS** — all cluster guard tests passed, including optional EndpointSlice denial. No Docker/Kubernetes operations were performed by the suite. |
| `go vet ./...` | **PASS** — no diagnostics. |
| Live lab preflight | **BLOCKED** — `Assert-LabOwnership -Reader` confirmed the owned lab, but reader API calls (`auth can-i list pods` and `get pods`) returned `Unauthorized`. A direct retry of the reader-only Pod GET returned the same result. No credentials were refreshed. |

## Live scenarios

| Scenario | Status | Result |
|---|---|---|
| Stale misleading signal vs current exit/event | **NOT RUN** — blocked before fixture creation. The new fixture is deterministic, but its observed diagnosis and citations remain unverified. |
| Healthy Pod, current logs and unavailable previous logs | **NOT RUN** — blocked before fixture creation. Collector behavior, explicit selection and uncertainty are covered by regression tests only. |
| Service selector vs Pod labels / EndpointSlices | **NOT RUN** — blocked before fixture creation. Label/port projection, EndpointSlice targeting, and explicit forbidden handling are covered by unit tests only. No model diagnosis was observed. |

No validation fixtures were created, so no cleanup was needed. The existing `checkout` fixture, live RBAC, credentials, GPU settings, and global configuration were not changed. No local-model inference or paid API call was made.

## Modified files

- `internal/tui/model.go`, `internal/tui/evidence.go`, `internal/tui/view.go`, `internal/tui/evidence_test.go`
- `internal/core/types.go`
- `internal/kube/backend.go`, `internal/kube/resources.go`, `internal/kube/backend_test.go`, `internal/kube/crashloop_test.go`
- `internal/agent/agent.go`, `internal/agent/tools.go`, `internal/agent/agent_test.go`, `internal/agent/crashloop.go`, `internal/agent/crashloop_test.go`
- `labs/crashloop/Cluster.psm1`, `labs/crashloop/cluster.Tests.ps1`, `labs/crashloop/manifests/rbac.yaml`
- `labs/crashloop/manifests/validation-stale-current.yaml`

## Limitation / next step

Retry the bounded GPU/Qwen lab scenarios only after the existing `kubix-lab-reader` credential authenticates again. This pass did not modify or renew that credential. The current lab API state and live scenario outcomes remain unknown.
