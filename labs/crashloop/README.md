# CrashLoop incident lab

A real Kubernetes Deployment repeatedly exits because `DATABASE_URL` is missing.
Use Kubix to collect the workload chain, inspect the application's actual previous
logs, and ask a local model to explain the captured evidence. The application is
a deliberately small fixture; it needs no database and contains no credentials.

The lab is intended for a short platform/SRE portfolio demonstration. Its scope
is a known configuration failure, not a diagnostic benchmark or production incident.

## Requirements

- Docker with **Linux containers**, running on this machine.
- PowerShell 7.2+ (`pwsh`), Go 1.26+, and `kubectl` 1.34 or 1.35 on PATH.
- Allow roughly 12 GiB of Docker memory and 20 GiB of free disk space for the
  cluster, model server, weights, and build cache. First setup downloads several
  GB; the pinned Ollama image alone occupies about 8.5 GB after extraction.
- Internet access for the initial downloads; cluster/model execution is local.

Commands below work from the repository root. The bootstrap installs a checksum-
verified kind v0.31.0 under `.local/bin`; no system installation or PATH change is
required. The node and fixture images are pinned by digest. The model server and
model manifest are pinned in [model-lock.json](model-lock.json).

## Prepare before the demo

```powershell
pwsh ./labs/crashloop/lab.ps1 up
pwsh ./labs/crashloop/lab.ps1 model-up
pwsh ./labs/crashloop/lab.ps1 verify
pwsh ./labs/crashloop/lab.ps1 run
```

`up` creates the dedicated `kubix-lab` kind cluster, applies the broken checkout
Deployment, waits for an actual CrashLoop, and creates a separate reader kubeconfig.
It can be rerun to renew credentials and restore the broken scenario.

`model-up` starts the dedicated `kubix-lab-model` Ollama container on
`127.0.0.1:11435`, disables cloud features, downloads the locked `qwen2.5:3b` model,
and checks its OpenAI-compatible endpoint. CPU inference is the portable default;
no GPU configuration is changed. Kubix's lab launch allows 300 seconds per model
request and 360 seconds per investigation. General launches keep the shorter defaults.

`verify` uses the reader credentials to capture real evidence and runs one model
summary over that capture. It checks exact source references and whether the
model cites the known missing-setting evidence. An evidence-only fallback does
**not** pass the model check. Reports are written to `.local/lab/validation.json`
and `.local/lab/model-validation.json`; the latter proves endpoint compatibility,
not diagnostic quality. Reports include timestamps and measured timings.
The incident summary requests schema-constrained output and still validates every
returned citation in Kubix. The report includes bounded, redacted model-response
diagnostics for rejected answers; it does not save kubeconfig credentials or the
full evidence prompt.

To investigate without inference, use `verify -WithoutModel` and
`run -WithoutModel`. This validates collection and citations only.

## Windows GPU demo

The Docker model runtime above is the portable CPU baseline. For an AMD GPU on
Windows, run the pinned Ollama server natively while keeping Kubernetes in kind:

```powershell
pwsh ./labs/crashloop/lab.ps1 model-up -ModelRuntime windows-gpu
pwsh ./labs/crashloop/lab.ps1 verify -ModelRuntime windows-gpu
pwsh ./labs/crashloop/launch.ps1 -NewWindow -Color -ModelRuntime windows-gpu
```

This installs the verified portable runtime under `.local/tools/ollama`, stores
weights under `.local/lab/ollama-models`, and binds only `127.0.0.1:11436`. It uses
the same pinned model and read-only cluster identity. GPU checks require an
actual GPU allocation; a silent CPU fallback fails the check. The helper does
not install drivers, register a service, or change global environment settings.
See [Ollama's supported GPUs and driver requirements](https://docs.ollama.com/gpu).
The GPU runtime unloads idle models after five minutes. Inference still competes
with games for GPU time and memory while active; use the stop command below
before gaming to release the server and its GPU workers immediately.

The dedicated launcher starts a 160-column by 45-row Windows Terminal window.
`-Color` explicitly enables true color for that session, including when an
automation parent supplies `NO_COLOR=1` or `TERM=dumb`. Normal `lab.ps1 run`
keeps your color preferences. Omit `-NewWindow` to use the current terminal;
override the new window size with `-Columns` and `-Rows`. Use `-ModelRuntime docker`
for the CPU server.

GPU investigation reports use `.local/lab/validation-windows-gpu.json`, leaving
the CPU report intact. To stop just the native GPU server, run:

```powershell
pwsh ./labs/crashloop/model-windows.ps1 -Action down
```

Then use the usual `lab.ps1 down` to remove the kind cluster and any Docker model
container. Native runtime files and weights remain cached for the next demo.

## Measured validation

The [GPU follow-up on the same Windows host](results/2026-11-13-gpu.json) passed
three real investigations in **1.66–1.81 seconds** of model time (median **1.77 s**),
using the RX 7900 XTX through ROCm. The model was already loaded, and all three
runs passed the known-failure citation check. This compares practical deployments
(CPU container versus native GPU), not isolated hardware performance. Native
shutdown was verified to stop the server and its GPU workers.

The [recorded Windows run on 2026-11-13](results/2026-11-13.json) passed with
**7 captured sources in 41 ms** and an accepted, cited model explanation in
**78.3 seconds**. Ollama used CPU inference with an 8-CPU quota, 8 GiB memory
limit, and 16,384-token context. All 19 reader permission checks passed. Operator
repair/reset, cleanup/recreation, and a real terminal capture/evidence inspection
were also exercised.

Earlier runs exposed invalid model output and then a structurally valid answer
that missed the required previous-log finding. Both failed verification. The
final path uses schema-constrained output, compact source metadata, and explicit
log guidance. The result records those attempts; one successful final run does
not establish a diagnostic success rate or a performance benchmark. Run `verify`
on your own machine before presenting.

## Two-minute walkthrough

Complete setup and let the first investigation finish before presenting if CPU
inference is slow. The capture and explanation can be inspected afterward; use
the recorded timing when describing how long the model actually took.

1. **Explain the boundary.** “Kubix has reader credentials. The lab operator
   creates and repairs the scenario separately.” In a second terminal, run:
   `pwsh ./labs/crashloop/lab.ps1 rbac`. Show allowed reads and denied writes,
   Secret reads, and pod exec.
2. **Investigate.** In Kubix, press `Esc`, select the checkout pod, and press `i`.
   Alternatively ask “Why is checkout restarting?” Point out the owning
   ReplicaSet and Deployment in the discovered resources.
3. **Check the explanation.** Click the citation beside the missing-setting log
   finding, or press `e` to choose evidence. Show the exact previous log line,
   capture time, pod UID, and sampling limits. The expected application message
   is `FATAL: required setting DATABASE_URL is missing`.
4. **Separate observation from action.** Leave that citation open. In the second
   terminal run `pwsh ./labs/crashloop/lab.ps1 repair`. This is an explicit
   operator action using the lab admin identity. It supplies a harmless fixture
   value and waits for a healthy rollout; Kubix performs no remediation.
5. **Demonstrate captured versus live state.** The old citation still shows the
   original failure. In Kubix choose Pods from the resource menu and refresh
   with `Ctrl+R`; inspect the replacement's healthy state with `d`. The old pod
   reference may report that it disappeared, which is expected after a rollout.

Finish with: “The model explains a bounded capture. Source identity, read-only
permissions, and visible gaps make its explanation inspectable; citations alone
do not prove a diagnosis.”

## Reset and cleanup

```powershell
# Reproduce the failure for another run; wait for an actual CrashLoop.
pwsh ./labs/crashloop/lab.ps1 reset

# Inspect only this lab's workload and reader permissions.
pwsh ./labs/crashloop/lab.ps1 status

# Remove the owned cluster and model container; retain downloaded model weights.
pwsh ./labs/crashloop/lab.ps1 down

# Also delete this lab's owned model volume when finished with it.
pwsh ./labs/crashloop/lab.ps1 down -RemoveModels
```

The scripts reject unrelated clusters/containers using the same names and remote
Docker endpoints. Cluster credentials and ownership metadata stay in the ignored
`.local/lab` directory. They do not change your usual kubeconfig or current context.
Downloaded Docker images remain cached for repeat runs.
The reader Role grants only the namespace reads needed for this workflow; it
does not grant node access, Secrets, exec, port-forward, or workload writes.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Docker unavailable | Start Docker with Linux containers, then rerun the command. |
| Image pull failure | Check registry access and disk space. The script does not substitute an unpinned image. |
| Credentials expired | Rerun `up` to renew the short-lived reader token and reset the incident. |
| No previous logs or identity changed | Wait for the next backoff interval and investigate again. Kubix discards samples whose identity cannot be verified. |
| Model timeout or invalid summary | Run `model-check`; check available memory/CPU. Captured observations remain available in Kubix. |
| Verification says model fallback | The model test failed even if collection passed. Keep that distinction in demo claims. |
| Plain or monochrome UI | Use `launch.ps1 -Color`; an inherited `NO_COLOR` setting can disable the palette even in Windows Terminal. |
| GPU check reports CPU fallback | Check the native server logs and installed drivers; the helper requires a GPU allocation before declaring the GPU demo ready. |
| Ownership mismatch | Stop and inspect the existing lab resources. The script intentionally refuses to adopt or delete unrelated resources. |
| `verify` after repair | Run `reset` first: validation expects the known broken scenario. |

The default model has its own [Qwen license](https://ollama.com/library/qwen2.5:3b);
this repository does not redistribute model weights. Record the actual model,
hardware, report timestamp, and observed behavior when sharing validation results.

## Sources and maintenance

- [kind v0.31.0 release and node digests](https://github.com/kubernetes-sigs/kind/releases/tag/v0.31.0)
- [Kubernetes namespace RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Short-lived ServiceAccount tokens](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/)
- [Ollama Docker setup](https://docs.ollama.com/docker) and [local-only settings](https://docs.ollama.com/faq)

When changing a pin, rerun the script guard tests and full live validation. Update
recorded results from that run; do not imply a newer pin was tested automatically.
