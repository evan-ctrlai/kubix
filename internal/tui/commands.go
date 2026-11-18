package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"sigs.k8s.io/yaml"

	"kubix/internal/core"
	"kubix/internal/redact"
)

type listResultMsg struct {
	generation uint64
	resources  []core.Resource
	errors     []string
	limited    bool
	nextCursor string
}
type detailResultMsg struct {
	generation uint64
	ref        core.ResourceRef
	text       string
	containers []string
	container  string
}
type answerMsg struct {
	generation uint64
	answer     core.Answer
}
type progressMsg struct {
	generation uint64
	text       string
	events     <-chan string
}
type finishedMsg struct{}

func waitProgress(ctx context.Context, generation uint64, events <-chan string) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return finishedMsg{}
		case text, ok := <-events:
			if !ok {
				return finishedMsg{}
			}
			return progressMsg{generation, text, events}
		}
	}
}

func askCommand(ctx context.Context, investigator core.Investigator, generation uint64, question string, selected *core.ResourceRef, events chan string) tea.Cmd {
	return answerCommand(ctx, generation, events, func(onEvent func(string)) core.Answer {
		return investigator.Ask(ctx, question, selected, onEvent)
	})
}

func crashLoopCommand(ctx context.Context, investigator core.Investigator, generation uint64, question string, selected core.ResourceRef, container string, events chan string) tea.Cmd {
	if typed, ok := investigator.(core.CrashLoopInvestigator); ok {
		return answerCommand(ctx, generation, events, func(onEvent func(string)) core.Answer {
			return typed.InvestigateCrashLoop(ctx, selected, container, onEvent)
		})
	}
	return askCommand(ctx, investigator, generation, question, &selected, events)
}

func answerCommand(ctx context.Context, generation uint64, events chan string, run func(func(string)) core.Answer) tea.Cmd {
	return func() (message tea.Msg) {
		defer close(events)
		defer func() {
			if recover() != nil {
				message = answerMsg{generation, core.Answer{Text: "The investigation could not finish. Check model configuration and cluster connectivity. Resource browsing remains available."}}
			}
		}()
		answer := run(func(text string) {
			select {
			case events <- redact.Text(text, 250):
			case <-ctx.Done():
			default:
			}
		})
		return answerMsg{generation, answer}
	}
}

func listCommand(ctx context.Context, backend core.Backend, generation uint64, kind, namespace string, refs []core.ResourceRef, cursors ...string) tea.Cmd {
	return func() (message tea.Msg) {
		defer func() {
			if recover() != nil {
				message = listResultMsg{generation: generation, errors: []string{"Unable to refresh. Check context, scope, and read permissions."}}
			}
		}()
		result := listResultMsg{generation: generation}
		if kind != "discovered" {
			var read core.ToolResult
			if pager, ok := backend.(core.ResourcePageReader); ok {
				cursor := ""
				if len(cursors) > 0 {
					cursor = cursors[0]
				}
				page := pager.ListResourcesPage(ctx, kind, namespace, "", 50, cursor)
				read, result.nextCursor = page.Result, page.NextCursor
			} else {
				read = backend.ListResources(ctx, kind, namespace, "", 50)
			}
			result.resources, result.errors, result.limited = read.Resources, read.Errors, limited(read)
			return result
		}
		result.limited = len(refs) > 50
		for _, ref := range refs[:min(len(refs), 50)] {
			if ctx.Err() != nil {
				return finishedMsg{}
			}
			read := backend.GetResource(ctx, ref.Kind, ref.Namespace, ref.Name)
			result.resources = append(result.resources, read.Resources...)
			result.errors = append(result.errors, read.Errors...)
		}
		return result
	}
}

func limited(result core.ToolResult) bool {
	return result.Data["limited"] == true || result.Data["truncated"] == true
}

func detailCommand(ctx context.Context, backend core.Backend, generation uint64, ref core.ResourceRef, mode, container string) tea.Cmd {
	return func() (message tea.Msg) {
		result := detailResultMsg{generation: generation, ref: ref}
		defer func() {
			if recover() != nil {
				result.text = "Unable to load " + mode + ". Check cluster connectivity and read permissions."
				message = result
			}
		}()
		details := backend.GetResource(ctx, ref.Kind, ref.Namespace, ref.Name)
		if replaced(ref, details) {
			result.text = "This resource was replaced since discovery. Refresh the resource list to inspect its replacement."
			return result
		}
		if len(details.Resources) == 0 && len(details.Errors) > 0 {
			result.text = resultText(details, "details")
			return result
		}
		payload := details.Data
		if len(details.Resources) > 0 {
			payload = details.Resources[0].Details
		}
		if ref.Kind == "pods" {
			result.containers = containerNames(payload)
		}
		found := false
		for _, name := range result.containers {
			if name == container {
				found = true
				break
			}
		}
		if !found {
			container = ""
			if len(result.containers) > 0 {
				container = result.containers[0]
			}
		}
		result.container = container
		read := details
		if ctx.Err() != nil {
			return finishedMsg{}
		}
		if mode != "details" {
			read = backend.PodLogs(ctx, ref.Namespace, ref.Name, container, mode == "previous")
			if replaced(ref, read) {
				result.text = "This resource was replaced while loading. Refresh the resource list before inspecting it."
				return result
			}
		}
		result.text = resultText(read, mode)
		return result
	}
}

func replaced(ref core.ResourceRef, result core.ToolResult) bool {
	if ref.UID == "" {
		return false
	}
	if len(result.Resources) > 0 && result.Resources[0].Ref.UID != ref.UID {
		return true
	}
	if uid, ok := result.Data["pod_uid"].(string); ok && uid != "" && uid != ref.UID {
		return true
	}
	return false
}

func containerNames(details map[string]any) []string {
	// Normalize either []map[string]any or JSON-decoded []any without aliases.
	data, _ := json.Marshal(details)
	var normalized map[string]any
	_ = json.Unmarshal(data, &normalized)
	spec, _ := normalized["spec"].(map[string]any)
	groups := []any{normalized["containers"], normalized["init_containers"], normalized["initContainers"], spec["containers"], spec["initContainers"]}
	var names []string
	seen := map[string]bool{}
	for _, group := range groups {
		values, _ := group.([]any)
		for _, value := range values {
			name, _ := value.(string)
			if item, ok := value.(map[string]any); ok {
				name, _ = item["name"].(string)
			}
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

func resultText(result core.ToolResult, mode string) string {
	var text string
	if mode == "logs" || mode == "previous" {
		for _, key := range []string{"logs", "text", "content"} {
			if value, ok := result.Data[key]; ok {
				text = fmt.Sprint(value)
				break
			}
		}
		if text == "" {
			text = "No log lines returned in the bounded time window."
		}
		if result.Data["bounded_sample"] == true {
			text += "\n\n[Bounded log sample; older lines may be omitted.]"
		}
	} else {
		var payload any = result.Data
		if len(result.Resources) > 0 {
			payload = result.Resources[0].Details
		}
		encoded, err := yaml.Marshal(redact.Value(payload, 16000))
		if err != nil {
			text = "Unable to format resource details."
		} else {
			text = strings.TrimRight(string(encoded), "\n")
		}
	}
	if len(result.Errors) > 0 {
		errors := make([]string, len(result.Errors))
		for i, message := range result.Errors {
			errors[i] = redact.Text(message, 2000)
		}
		if mode == "logs" || mode == "previous" {
			text += "\n\nEvidence gaps:\n- " + strings.Join(errors, "\n- ")
		} else {
			text += "\n\n# Evidence gaps:\n# - " + strings.ReplaceAll(strings.Join(errors, "\n- "), "\n", "\n# ")
		}
	}
	if mode != "logs" && mode != "previous" {
		// Values were redacted before YAML serialization. Pattern-redacting the
		// document again would remove quotes and change scalar types.
		return redact.Sanitize(text, 32000)
	}
	return redact.Text(text, 32000)
}
