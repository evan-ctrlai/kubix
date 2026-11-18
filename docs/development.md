# Development

Kubix is a Go application. The terminal uses Bubble Tea, Bubbles, Lip Gloss, and
Glamour. Kubernetes access uses client-go; the model adapter uses `net/http`.
There is no Python runtime, database, or background service in the executable.

## Packages

| Package | Responsibility |
| --- | --- |
| `cmd/kubix` | Executable entry point |
| `internal/cli` | Flags and application startup |
| `internal/tui` | Terminal layout, focus, navigation, asynchronous commands |
| `internal/core` | Resource types, captured observations, citation selectors, read-only interfaces |
| `internal/kube` | Scoped, bounded client-go reads |
| `internal/agent` | Model requests, strict tool dispatch, evidence and citations |
| `internal/demo` | Embedded synthetic cluster, independent of credentials |
| `internal/labtest` | Opt-in real incident validation and bounded reports |
| `internal/config` | Explicit model configuration and offline-demo defaults |
| `internal/redact` | Best-effort credential and terminal-control filtering |

## Checks

```sh
go test ./...
go vet ./...
go build ./cmd/kubix
```

Use `go test -race ./...` on a host with a supported C compiler. Tests exercise
the terminal's Update/command cycle, synthetic Kubernetes clients and local HTTP
servers; they never connect to a real cluster or model. HTTP tests require local
loopback socket access.

The incident lab scripts have separate offline guard tests:

```sh
pwsh ./labs/crashloop/test.ps1
```

The [real incident lab](../labs/crashloop/README.md) creates an isolated kind
cluster and local model. Its live Go checks are opt-in through `lab.ps1 verify`;
ordinary `go test ./...` never connects to that cluster or downloads weights.
The live report distinguishes source/citation validation from model acceptance
and from general diagnostic accuracy, which this single fixture cannot establish.

Keep generated binaries out of source control. Dependencies are pinned in
`go.mod` and verified with `go.sum`. The CI workflow is configured to test and build on Windows, Linux, and
macOS. Resource fields and UI/model output must remain bounded and sanitized.

## Behavior preserved from the prototype

- The same embedded demo pods, deployments, services, event, node, and logs.
- Questions include the currently selected resource and short conversation history.
- Only list, detail, pod-log, and captured pod-investigation tools are available;
  scope is enforced in code.
- Model replies must cite references actually transmitted in tool output.
- Empty/error answers preserve the resource browser.
- Asynchronous results cannot overwrite a newer selection or display a replacement
  resource under an old UID.
- Namespace/context configuration never changes the user's current kubeconfig.

The Go port carries over behavioral tests rather than depending on the Python
implementation. The previous local prototype is retained in the gitignored
`.local/python-mvp-backup` directory after migration verification.

## Captured CrashLoop investigations

`CrashLoopCollector` is an optional backend capability; `CrashLoopInvestigator`
is a typed terminal entry point that preserves the selected pod UID and an
explicit container choice. The ordinary backend interface remains read-only.
The real collector uses a fixed plan with bounded reads, controller owner UID
checks, exact pod-UID event filtering, and a final identity check before retaining
log samples. No Secret contents, environment values, arbitrary commands, or
mutation operations are exposed.

An `Investigation` owns serialized, sanitized observations and typed citation
selectors. Structured observations contain valid JSON for reliable field
addressing; the terminal renders them as YAML. A citation resolves either a full
observation, an RFC6901 JSON pointer, or an inclusive range of log line numbers.
Payload strings do not change when live resources refresh. Each conversation
turn clones the capture and maintains its own citation anchors, so an older
`[E1]` cannot accidentally resolve against a newer answer. Opening evidence
cancels pending inspector reads and protects the captured view from stale replies.

The agent creates exact field and line targets before calling the model. A
single summarization request has no tools and uses structured claims with cited
IDs. The HTTP adapter requests JSON Schema output through the optional
`StructuredModel` capability, restricting citation IDs to transmitted, usable
sources. The renderer independently validates the returned response even if a
server ignores the schema. General chat and tool requests keep their usual format;
an unsupported structured-output request falls back without an extra model call.
Only complete targets transmitted within the model context budget can be
cited. Source metadata is serialized once per observation; focused targets avoid
repeating the full source payload. Full captures remain available in the UI, but
their broad citation IDs are excluded from model output when those payloads were
not transmitted. Unknown IDs, unavailable/changed evidence, malformed output, or model
failure cause a deterministic fallback. This verifies reference integrity, not
semantic entailment or diagnostic accuracy. General question tools retain their
existing bounded loop and resource references.

Tests cover controller traversal, forbidden/missing reads, replaced UIDs and
container instances, event identity, cancellation, ambiguous containers,
read-only client actions, immutable captures, model summary rejection, and
click/keyboard citation navigation after resize and scroll. The embedded demo
exercises the same capture and answer interfaces without a model or cluster.

