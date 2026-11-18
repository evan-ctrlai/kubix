# Kubix bounded live scenario validation — 2026-11-18

Executed 16:48:40–16:48:48 UTC against owned kubix-lab with existing reader permissions and local qwen2.5:3b on Windows GPU. No RBAC expansion, paid API, production access, or application source changes.

## Results
- Stale versus current signals: FAIL diagnostic interpretation. Both lines were captured together from current logs, and prior logs were explicitly unavailable. Model nevertheless described them as previous logs and treated the explicitly STALE missing-DATABASE_URL claim as a plausible current cause alongside the current release-signature failure. Valid citations did not prevent incorrect temporal interpretation.
- Healthy Pod / missing prior logs: capture PASS; conservative model interpretation FAIL. Current logs, Ready state and zero restarts were captured with a prior-log-unavailable gap. Model speculated that missing logs indicated recovery from an earlier failure, which the fixture did not establish.
- Broken Service / denied EndpointSlices: evidence-boundary PASS; useful diagnosis FAIL. Service selector app=validation-wrong and Pod label app=validation-real were retrieved. EndpointSlice denial remained an explicit 403 error. Model did not cite retrieved resources, so Kubix discarded the answer rather than displaying unsupported diagnosis.

These were one run each, not an accuracy benchmark. Tests exercise real backend/agent code headlessly; no new terminal UI validation was performed. Source citation checks establish provenance, not semantic correctness.

The disposable fixtures were created with lab-admin identity and read by Kubix's scoped reader. Cleanup deleted only the two new Pods and new Service. No original workload, RBAC, global kubeconfig or GPU setting was changed.

Local detailed evidence and reproducible harness: .local/scenarios-20261118/result.json, main.go, run.ps1, fixture manifests. These are ignored local files, not published.

Baseline checks in same session: Go tests, vet, build PASS; full offline script guard suite PASS outside sandbox; live baseline CrashLoop capture PASS with seven sources in 43 ms. Sandbox ACL failure was not reproduced outside sandbox. Reader renewal preserved existing privileges; EndpointSlice reads remain denied.

## Remediation in progress
The first two failures are now handled conservatively: ambiguous temporal narratives and healthy/zero-restart captures with unavailable history bypass model diagnosis and show captured source evidence. Current termination reason/exit code receive precise citations. The typed Pod investigation request no longer assumes restarting. New regression tests verify the guards do not invoke the model and retain evidence.

First rerun (16:52:18–16:52:19 UTC) confirmed both conservative outputs. Service model returned empty before tool use; deterministic selected-Service routing inspection is being added rather than describing this as model success. Before/after raw runs are preserved locally.

## Final remediation and revalidation
Final scenario run at 16:55:04–16:55:05 UTC:
- Stale/current: conservative handling PASS. Explicit temporal conflict produces evidence-only output. Current termination Error / exit 42 and both exact current-log lines are cited. No stale explanation adopted as a current diagnosis.
- Healthy/missing logs: conservative handling PASS. Ready/zero restarts plus missing prior logs produces evidence-only output, explicitly stating earlier failure or recovery cannot be established.
- Selected-Service routing: deterministic evidence workflow PASS. Three bounded sanitized reads establish selected Service identity, list at most 30 namespace Pods, and attempt EndpointSlices. Reports the actual app selector mismatch with citations and retains denied EndpointSlice evidence. No model call or permission expansion is needed for this path.

Final go test ./..., go vet ./..., and build all PASS. New unit tests cover temporal gates, ordinary CrashLoop non-suppression, current termination citations, Service exact/stale identity, scope, cancellation, truncation, missing labels, cross-namespace exclusion and retained 403 gaps.

An intermediate ordinary CrashLoop model regression check failed its exact previous-log citation requirement (capture succeeded; model chose other log citations). Restored explicit previous-log prioritization in the summary prompt, retaining temporal distinctions. Final ordinary real CrashLoop model verification PASS: seven sources in 34 ms, one local model summary in 1884 ms citing the expected failure. The failed intermediate report remains at .local/scenarios-20261118/baseline-model-first-remediation.json. A final pass does not establish a general success rate.

Changes intentionally improve deterministic boundaries, not assert general model reasoning quality. Non-selected generic Service questions and novel ambiguous incidents can still depend on model quality. No new UI acceptance test, cross-platform CI, publication or commit was performed. Source checkpoint before changes is retained; updated checkpoint is separate. Reader permissions remain unchanged.
