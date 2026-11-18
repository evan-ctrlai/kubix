# Kubix messy incident validation — 2026-11-13

## Scope and safety

- Disposable context: `kubix-lab-reader`; namespace: `kubix-lab`.
- Kubix used its dedicated reader kubeconfig and local GPU `qwen2.5:3b` endpoint. No paid API was used.
- All 19 read-only permission checks passed, including denied create/patch/delete/exec and cross-namespace access.
- Only pass-labeled disposable Pod/ConfigMap fixtures were created. No code, RBAC, credentials, GPU settings, or pre-existing workload configuration was changed.

## Results

| Case | Ground truth / expected | Observed | Evidence and limits | Elapsed |
|---|---|---|---|---|
| Conflicting stale/current signals | First container logged a stale DATABASE_URL claim; desired current failure was exit 42 with a release-signature error. Kubix should favor current termination and events. | **Fixture setup failed; no Kubix diagnosis was run.** The first attempt reached the new failure after the stale log had already rolled out of the previous-container slot. A bounded retry hit Kubernetes restart backoff before the matching evidence window. | Stale and current lines were observed at different times, but not together in one captured Kubix investigation. No citations or diagnosis to score. | About 6 minutes of setup attempts; zero model calls. |
| Missing prior logs / healthy Pod | Pod `kubix-validation-20261113a-loggap` was Ready, restart count 0, and logged only `healthy replacement running; no diagnostic history exists in this container`. `kubectl logs --previous` returned `BadRequest: previous terminated container ... not found`. Expected: say current state is healthy, prior failure cannot be determined, and avoid inventing a cause. | **Partial pass.** Kubix reported a Ready Pod with no restarts and made no causal claim about a prior failure. Its collector recorded `E3 Container selection` as unavailable: “No container is currently in CrashLoopBackOff; choose a container explicitly if historical logs are needed.” The investigation did not fetch current or previous logs, so this did not test model reasoning over an unavailable-log observation. | Answer cited `[E7]` Pod summary and `[E9]` restart count; both appeared in the captured-evidence picker. `E3` is the explicit collection gap. Ground-truth previous-log unavailability was confirmed with the reader identity. | About 9 seconds from pressing `i` to the completed answer; roughly 43 seconds from Pod creation to evidence capture. |
| Healthy Pod behind broken Service | Would require comparing a Service selector/target port with Pod labels or endpoints. | **Unsupported; not attempted.** | Kubix exposes Service selectors/ports, but its Pod resource projection omits Pod labels, and it has no Endpoints/EndpointSlice kind. A valid diagnosis would require expanding existing evidence capabilities, which was out of scope. | Not run. |

## Reproduce the completed case

From the repository root, first verify lab ownership and reader permissions:

```powershell
Import-Module ./labs/crashloop/Cluster.psm1 -Force
$paths = Get-LabPaths
Assert-LabOwnership -Reader
Test-LabReadOnly
```

The temporary Pod used this manifest (apply with the **admin** kubeconfig only to the verified lab; Kubix itself must use the reader kubeconfig):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: kubix-validation-20261113a-loggap
  namespace: kubix-lab
  labels:
    kubix-validation-pass: 20261113a
spec:
  automountServiceAccountToken: false
  restartPolicy: Always
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: app
    image: busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
    command: ["/bin/sh", "-c"]
    args:
    - |
      echo "healthy replacement running; no diagnostic history exists in this container"
      while true; do sleep 30; done
    resources:
      requests: {cpu: 5m, memory: 8Mi}
      limits: {cpu: 100m, memory: 32Mi}
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
```

Save that block as `.local/lab/validation-loggap.yaml`, then run:

```powershell
kubectl --kubeconfig $paths.AdminConfig --context kind-kubix-lab -n kubix-lab apply -f .local/lab/validation-loggap.yaml
kubectl --kubeconfig $paths.ReaderConfig --context kubix-lab-reader -n kubix-lab get pod kubix-validation-20261113a-loggap -o wide
kubectl --kubeconfig $paths.ReaderConfig --context kubix-lab-reader -n kubix-lab logs kubix-validation-20261113a-loggap -c app
kubectl --kubeconfig $paths.ReaderConfig --context kubix-lab-reader -n kubix-lab logs kubix-validation-20261113a-loggap -c app --previous
pwsh -NoProfile -File ./labs/crashloop/lab.ps1 run -ModelRuntime windows-gpu
```

In Kubix, refresh Pods, select the named fixture, and press `i`; inspect the captured evidence (`e`) and the `[E7]`/`[E9]` citations. A Ready single-container Pod currently produces the `E3` selection gap, so no logs are sampled.

## Cleanup and verification

Only delete the exact objects created by the pass:

```powershell
kubectl --kubeconfig $paths.AdminConfig --context kind-kubix-lab -n kubix-lab delete pod kubix-validation-20261113a-loggap kubix-validation-20261113a-conflict --ignore-not-found=true --wait=true
kubectl --kubeconfig $paths.AdminConfig --context kind-kubix-lab -n kubix-lab delete configmap kubix-validation-20261113a-conflict-config --ignore-not-found=true --wait=true
kubectl --kubeconfig $paths.ReaderConfig --context kubix-lab-reader -n kubix-lab get pods -l kubix-validation-pass=20261113a -o name
```

The final command returned no Pods. Admin exact-name checks also confirmed both Pods and the ConfigMap absent. The original `checkout` Deployment, ReplicaSet, and CrashLoop Pod remained in place. Temporary manifest files and their empty working directory were removed.
