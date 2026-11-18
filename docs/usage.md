# Usage reference

Kubix is an AI-first, read-only Kubernetes terminal application written in Go.
The interface uses Bubble Tea; Kubernetes access uses client-go. Browse cluster
resources, inspect a pod's logs, and ask questions against live cluster evidence
without leaving the terminal.

This is an early local application, not a complete replacement for k9s. The first
version focuses on resource inspection and grounded investigation. It has no
Kubernetes mutation tools, shell execution, pod exec, or port forwarding.

## Install

Go 1.26 or newer is required to build from source. The resulting executable
runs without Go or Python installed.

```sh
# Linux / macOS
go build -o bin/kubix ./cmd/kubix
./bin/kubix --demo
```

```powershell
# Windows PowerShell
go build -o bin/kubix.exe ./cmd/kubix
.\bin\kubix.exe --demo
```

The examples below use `kubix` for brevity. Use the path to your executable, or
add its directory to your `PATH`.

## Try the demo

```sh
kubix --demo
```

The demo uses a synthetic cluster and scripted investigation responses, so it
does not require a cluster or model server. It demonstrates the UI and evidence
flow; it is not a model-quality benchmark.

To test a real model against synthetic cluster evidence, explicitly provide
`--model-url` and `--model` along with `--demo`. Without that explicit endpoint
argument, demo mode remains offline even if model environment variables are set.

For a reproducible **real Kubernetes incident**, use the [CrashLoop incident
lab](../labs/crashloop/README.md). It includes an isolated kind cluster, a deliberately
broken Deployment, read-only credentials, a local Ollama model, validation reports,
and a short demo runbook.

## Browse a cluster

Kubix uses your local kubeconfig and its configured authentication. Select the
context and namespace explicitly when connecting to a real cluster:

```sh
kubix --context my-cluster --namespace my-app
```

If omitted, the context comes from your kubeconfig. Namespace scope uses that
context's namespace, falling back to `default` when unset. An explicit
`--namespace` remains fixed across context switches. Use
`--kubeconfig /path/to/config` for a different configuration file.
Use `--all-namespaces` to explicitly expand the scope of namespaced resources.
Nodes are cluster-scoped, and access still depends on your Kubernetes
permissions.

Press **Ctrl+K** to choose a context in the loaded kubeconfig; Enter opens it
and Esc cancels. Context and effective namespace remain visible. Switching
cancels old work and clears resources, conversation and captured evidence rather
than mixing clusters. It does not update kubeconfig `current-context`. Kubix
still operates on one context at a time, not a fleet-wide aggregate. Only use
trusted kubeconfig files: their authentication plugins may execute local helpers.

In the resource pane, **]** loads the next page and **[** returns to a previous
page. Each page replaces the rows rather than accumulating an unbounded list.
Ctrl+R starts again from the first page. Paging tokens can expire as cluster
state changes; refresh rather than treating an incomplete page as a full inventory.

Resource browsing, log inspection, and captured CrashLoop investigations work
without a model endpoint. General questions require a configured model.

## Connect an open-weight model

Run a model server that supports the OpenAI-compatible chat-completions API and
tool calling, then point Kubix at its `/v1` base URL. Kubix does not download,
train, or serve model weights itself.

```sh
kubix --context my-cluster --namespace my-app \
  --model-url http://localhost:8000/v1 --model YOUR_SERVED_MODEL_NAME
```

The command above uses POSIX line continuation; in PowerShell, enter it on one
line. The served model must reliably produce tool calls using JSON arguments.
Model quality, context length, hardware needs, and tool-call compatibility depend
on your selected model and server configuration.

CrashLoop summaries use the server's `response_format: json_schema` support,
with tools disabled. The pinned Ollama server in the incident lab supports this.
If your server rejects structured output, Kubix retains the captured observations
and reports that the model summary was unavailable.

For slower local inference, set explicit deadlines such as `--model-timeout 300s
--investigation-timeout 360s`. Defaults are 30 seconds per model request and 90
seconds per investigation. The overall deadline must cover the model deadline;
CrashLoop collection retains its separate 60-second bound.

Alternatively, set these environment variables in the shell that runs Kubix:

| Variable | Purpose |
| --- | --- |
| `KUBIX_MODEL_BASE_URL` | Explicit model API base URL, such as `http://localhost:8000/v1` |
| `KUBIX_MODEL_NAME` | Model identifier exposed by your server |
| `KUBIX_MODEL_API_KEY` | Optional API key for that endpoint |

See [`.env.example`](../.env.example) for example values. It is a reference file;
export the variables into your shell environment before starting Kubix.

There is no default hosted model endpoint. When you configure an endpoint and
ask a question, the question and selected Kubernetes evidence can be sent to
that endpoint. Choose a local server if those requests must remain on your
machine. Credentials for the Kubernetes API are handled by the Kubernetes
client, not included as model tools.

## Use the terminal

Use a terminal at least 65 columns wide and 22 rows tall. A wider window gives
the conversation and resource names more room.

| Key | Action |
| --- | --- |
| `Esc` | Focus the resource table |
| `Tab` / `Shift+Tab` | Cycle question, resources, inspector, and conversation panes |
| `↑` / `↓` | Select a resource |
| `Enter` | Inspect the selected resource |
| `d` | Show resource detail |
| `l` | Read selected pod logs |
| `p` | Read the selected pod's previous container logs |
| `i` | Investigate the selected pod's CrashLoop and capture its evidence |
| `e` | Choose captured evidence from current or earlier answers |
| `k` | Cycle resource kinds and AI discoveries |
| `c` | Cycle containers in the selected pod |
| `/` | Focus the question input |
| `Ctrl+R` | Refresh resources |
| `Ctrl+Q` | Quit |
| `?` | Show help |

Letter shortcuts apply outside the question input. For pods with several
containers, press `c` or click the container control above the log panel.
The Details panel displays resource fields as YAML; log views show plain text.
Press `k` to cycle resource kinds, or click the selector in the resource header
to choose pods, deployments, services, events, nodes, or the latest AI discoveries.
Use arrow keys and Enter in the open selector; Esc dismisses it. Clicking a resource opens
its details; the mouse wheel scrolls the pane under the pointer. Focus a pane
with `Tab` to scroll with arrow or page keys.

Try questions such as:

- “Why is the checkout pod restarting?”
- “Which deployments have unavailable replicas?”
- “Show the warning events related to this pod.”

The general agent can list pods, deployments, services, events, and nodes;
inspect a resource; read bounded pod logs; and request a captured pod
investigation. General questions allow at most eight tool calls and eight model
requests, with a default 90-second context deadline propagated to Kubernetes and model
HTTP requests. Quitting cancels pending work.
Answers should distinguish observed facts from hypotheses. Resource citations
such as `[R1]` have a legend identifying the object; use the resource table to
inspect it.

## Investigate CrashLoopBackOff

Select a pod and press `i`, or ask “Why is the checkout pod restarting?” Kubix
collects a fixed evidence bundle before composing the answer:

1. The pod's UID, container states, restart counts, and last termination.
2. Its controlling ReplicaSet and Deployment, following exact owner UIDs.
3. Events associated with that pod UID, with sampling limits recorded.
4. Previous and current logs from the failing container, followed by a pod and
   container identity check. Samples are discarded if that identity changed.

Captured workload resources, including the owning ReplicaSet, also appear in the
AI discoveries table for navigation.

The failing container is chosen automatically when it is unambiguous. Press `c`
to choose a specific container before investigating. If several pods match a
question, Kubix asks you to select one rather than guessing.

Click an underlined `[E#]` citation in an answer to open its **captured** source.
Field citations show the exact field; log citations show the numbered lines.
The inspector includes capture time, UID, source, query, and any evidence gaps.
Press `e` for a keyboard-accessible evidence picker, including earlier answers;
use arrows and Enter to open a source. These actions do not make new cluster
reads. Press `d`, `l`, or `p` to return to live resource details or logs.

Without a model, Kubix reports the collected observations and suggested checks.
With a model, it makes one bounded, schema-constrained summary request with tools disabled, separates
observations from possible explanations, and accepts only citations to evidence
actually supplied in that request. An invalid or unavailable model summary falls
back to the collected observations. For two known ambiguity patterns, Kubix intentionally returns captured observations
without model diagnosis: a current-log sample containing an explicitly historical diagnostic claim alongside a separate failure,
and Ready zero-restart containers with unavailable prior logs. These conservative
fallbacks preserve uncertainty; they do not resolve the historical cause or prove
that a log statement is true. Current termination fields are shown separately
from the last termination of an earlier container instance.
Citations identify supporting source records;
they do not establish that a model's interpretation is correct.

Captures stay in memory for the terminal session; they are not saved to disk.
Each read has its own timestamp: this is a sequential collection, not an atomic
cluster snapshot. Permission errors, missing owners, expired logs, and changing
pods remain visible as gaps. Collection allows at most ten API operations over
60 seconds, with a ten-second deadline per read and the outer investigation
deadline still applying. The ordinary Pod → ReplicaSet → Deployment path uses
seven operations. Events use one page of at most 30 records; each structured
observation is capped at 64 KiB. No remediation is executed.

## Scope and limits

- Kubernetes access is read-only in the application. Using kubeconfig credentials
  with read-only RBAC provides an additional independent boundary.
- Namespaced reads stay within the selected namespace unless you explicitly use
  `--all-namespaces`.
- Resource updates use manual refresh. Kubix does not yet provide a watch-based
  view of all cluster changes.
- Each list request fetches one bounded API page of at most 100 resources;
  the browser uses 50 and supports explicit next/previous navigation with bounded
  cursor history. Agent text filtering still searches one page, so an empty
  result does not establish that a resource is absent from the cluster. Pod logs are samples bounded to the last 15 minutes,
  100 lines, and 16 KB.
- Generic custom resources, editing, remediation, terminal exec, port forwarding,
  and complete k9s feature parity are outside this version.
- Logs and resource text are evidence, not trusted instructions. Tool calls are
  validated, but a model can still misdiagnose a problem. Inspect the cited
  evidence before acting on a recommendation.
- There is no background incident responder, web API, database, or in-cluster
  service to deploy in this version.

## Develop

```sh
go test ./...
go vet ./...
go build ./cmd/kubix
```

Tests use synthetic data, client-go fake clients, and local HTTP test servers,
including headless terminal interaction and layout checks. They do not require
Kubernetes credentials, a model server, Python, or a GPU. Local loopback access
is needed for the HTTP tests. The CI workflow is configured to test and build on Linux, Windows, and macOS,
and runs the race detector on Linux.

See [development notes](development.md) for package boundaries and migration
behavior. The original Python prototype is archived locally after verification;
it is not part of the Go build or runtime.


## Selected-Service routing evidence

When a selected Service is the subject of a routing/selector/endpoint question,
Kubix uses at most three read-only API requests without a model. It verifies the
exact Service identity, then reads an explicitly named target Pod or queries
Pods with the Service equality selector on the server, and queries EndpointSlices
with the Service-name label. Each list is bounded to one page of 30; there is no
pagination. Exact target lookup is not limited to the first namespace page.

General inspection does not call unrelated workloads broken. Permission denials
and truncation remain explicit gaps. A selectorless Service skips Pod listing;
backends without server-side filtering disclose their incomplete namespace-page
fallback. See [full Service routing behavior and tests](service-routing.md).
