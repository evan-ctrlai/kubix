# Bounded Service routing inspection

Select a Service, then ask about routing, selectors, or endpoints. This path is deterministic and read-only: it does not require a model and does not execute remediation.

Three API reads maximum:
1. Read the selected Service by exact name/namespace. A supplied stale UID is rejected; an unqualified name is not searched across namespaces.
2. If the question explicitly names one Pod (`pod checkout-abc`, `pod/checkout-abc`, or `pod namespace/checkout-abc`), read that exact Pod in the Service namespace. Otherwise query Pods using the Service's equality selector on the Kubernetes server, bounded to one page of 30.
3. Query EndpointSlices using the server-side `kubernetes.io/service-name` label, bounded to one page of 30.

Explicit Pod comparison can explain a selector mismatch even when the target lies beyond the first page of a large namespace. General inspection does not treat unrelated workloads as broken. Matching label-filtered results are retained even when their displayed label projection is truncated.

Multiple named Pods or an explicit different namespace require a narrower question. A selectorless Service does not trigger a namespace-wide Pod search. Backends without server-side filtering use one bounded namespace page and explicitly report that incomplete fallback.

The report shows captured selector evidence and provenance, plus denied reads and incomplete-page warnings. A 403 is not proof of absent endpoints; no permission expansion is performed. Lists are not paginated, and these are sequential reads rather than an atomic cluster snapshot. A selector mismatch is not proof of the only routing fault. This workflow does not test connectivity, prove port correctness, or infer prior failures.

Tests use fake Kubernetes clients and synthetic resources for server-side selector application, 65-Pod namespaces, >30 matching resources, target-Pod exact lookup, scope/identity/cancellation guards, truncation, and permission gaps. They are not live-cluster diagnostic benchmarks.

## Explicit question targets

Use `Service NAME`, `Service named NAME`, `service/NAME`, or `service NAMESPACE/NAME` to name the Service; `svc` is also recognized. If that explicit target differs from the selected Service, the request stops before any read instead of silently inspecting the selection.

For exact Pod comparison use `Pod NAME`, `Pod named NAME`, `pod/NAME`, or `Pod NAMESPACE/NAME`. `Pod named NAME` and slash syntax are recommended when a resource name is also a common inspection word, such as `status` or `selector`. Ordinary phrases such as "pod readiness" do not become resource names. This is a bounded explicit-name grammar, not general natural-language entity resolution; multiple targets require a narrower question.
