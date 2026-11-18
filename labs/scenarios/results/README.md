# Recorded independent scenario runs

These reports were produced by the public `labs/scenarios/run.ps1` harness on
November 18, 2026 (18:07 UTC), against the owned local kind lab on Windows.
Each scenario was run separately with no configured model. All exact disposable
fixtures were removed using UID-precondition cleanup; original lab workloads and
reader permissions were unchanged. EndpointSlice reads remained denied.

| Scenario | Result | Investigation time | Captured / available source records | Rendered citation occurrences | Model requests |
| --- | --- | ---: | ---: | ---: | ---: |
| [Historical/current diagnostic conflict](2026-11-18-stale-current.json) | PASS: evidence-only uncertainty | 39 ms | 6 / 4 | 9 | 0 |
| [Healthy Pod, unavailable history](2026-11-18-healthy-history.json) | PASS: no invented earlier failure/recovery | 27 ms | 6 / 4 | 6 | 0 |
| [Service selector mismatch, EndpointSlice denial](2026-11-18-service-mismatch.json) | PASS: cited mismatch and explicit gap | 15 ms | 3 / 2 | 7 | 0 |

Use the JSON as the authoritative counts. Source records are not Kubernetes API
call counts; citation counts are occurrences, not an accuracy score. Timing
excludes fixture preparation, Go compilation and model startup. These are single
controlled runs, not percentile latency, production incidents or an accuracy
benchmark. Model behavior is not tested by these deterministic runs; see the
separate CrashLoop lab records for local-model validation and prior failures.

The reports contain only synthetic fixture identifiers and bounded, sanitized
answers, not kubeconfigs, credentials, machine paths or full cluster captures.

## Configured-model regression

A second independent pass at 18:09 UTC configured the running local qwen2.5:3b
endpoint. All three safeguards still made **zero model requests**: historical
conflict 34 ms, healthy/missing history 27 ms, Service mismatch/denial 14 ms.
See the corresponding `*-model-configured.json` reports. Cleanup succeeded.
This verifies bypass behavior, not model reasoning quality.

The separate ordinary CrashLoop regression also passed: seven captured sources,
29 ms capture, one local-model request in 3,907 ms, and a cited known missing
DATABASE_URL fixture claim. Its acceptance checks citation structure and that
known claim, not the correctness of every generated sentence. These single-run
results do not establish general accuracy or a latency SLA.
