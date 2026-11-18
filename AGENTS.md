# Project direction

Kubix is a focused portfolio project intended to demonstrate engineering judgment
through a useful, polished Kubernetes investigation tool. Favor a small number
of convincing workflows that someone can try and understand quickly.

The primary audience is platform engineering, SRE, and DevOps roles. Emphasize
Kubernetes troubleshooting, least-privilege access, operational reliability,
and tools that help an engineer make an informed incident response decision.

## Priorities

- Keep the offline demo easy to launch and suitable for a two-minute walkthrough.
- Make the CrashLoop workflow excellent: automatic workload collection, captured
  evidence, exact citations, clear gaps, and useful next checks.
- Establish reproducible real-cluster and local-model validation. Clearly separate
  synthetic demo behavior, automated tests, and results measured on a real setup.
- Build reproducible incident labs and concise investigation runbooks. Demonstrate
  behavior when permissions, logs, workload identity, or model availability change.
  Measure useful outcomes such as time to a supported finding and evidence coverage
  before making performance or reliability claims.
- Explain the architecture and consequential tradeoffs, including Go/Bubble Tea,
  client-go, read-only capabilities, evidence identity, and model fallbacks.
- Make the repository easy to evaluate: clear quickstart, representative visuals,
  focused tests, reproducible builds, and honest limitations.
- Choose additional features for the engineering story and user value they add.
  Keep setup and operational complexity appropriate for a side project.

## Existing product commitments

- Preserve the Go terminal UI, YAML details, keyboard/mouse navigation, and offline
  demo experience.
- Keep cluster access read-only. The model may recommend checks and fixes; the
  application does not execute remediation, arbitrary shell commands, or pod exec.
- Support explicitly configured local/open-weight model servers without requiring
  a hosted model account. Evidence collection must remain useful without a model.
- Preserve captured source identity and per-answer citations. A valid reference
  establishes provenance, not the correctness of a model's diagnosis.
- Prefer honest, demonstrable claims. Do not describe synthetic scenarios as
  production incidents or imply diagnostic benchmarks that have not been run.
