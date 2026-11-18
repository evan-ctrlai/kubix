package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"kubix/internal/core"
	"kubix/internal/redact"
	"regexp"
	"strings"
)

var routingQuestion = regexp.MustCompile(`(?i)\b(routes?|routing|selectors?|endpoints?|endpointslices?)\b`)

func serviceRoutingQuestion(question string, selected *core.ResourceRef) bool {
	return selected != nil && selected.Kind == "services" && routingQuestion.MatchString(question)
}

// investigateServiceRouting uses at most three bounded, sanitized reads. It does
// not depend on a model to choose tools, establish identity, or cite findings.
func (i *Investigator) investigateServiceRouting(ctx context.Context, selected core.ResourceRef, onEvent func(string)) core.Answer {
	if selected.Kind != "services" || selected.Name == "" || selected.Namespace == "" || len(selected.Name) > 253 || len(selected.Namespace) > 63 {
		return core.Answer{Text: "Select a Service with an explicit name and namespace."}
	}
	if scope := i.backend.Namespace(); scope != "" && selected.Namespace != scope {
		return core.Answer{Text: "The selected Service is outside the configured namespace scope."}
	}
	if ctx.Err() != nil {
		return core.Answer{Text: "Investigation cancelled or reached its time budget."}
	}
	resources := map[string]core.Resource{}
	order := []string{}
	refs := map[string]string{}
	evidence := []core.ToolResult{}
	add := func(result core.ToolResult) {
		evidence = append(evidence, result)
		for _, r := range result.Resources {
			key := r.Ref.Key()
			if _, ok := refs[key]; !ok {
				refs[key] = fmt.Sprintf("R%d", len(refs)+1)
				order = append(order, key)
			}
			resources[key] = r
		}
	}
	finish := func(note string) core.Answer {
		text := uncitedEvidenceSummary(resources, order, refs, evidence)
		text = strings.Replace(text, "The model did not cite its retrieved resources. Its answer was discarded.", "Read-only routing inspection of the selected Service. No model diagnosis was requested.", 1)
		if note != "" {
			text = note + "\n\n" + text
		}
		cited := citations(text)
		result := []core.Resource{}
		legend := []string{}
		for _, key := range order {
			r := resources[key]
			result = append(result, r)
			if cited[refs[key]] {
				legend = append(legend, fmt.Sprintf("- **[%s]** `%s`", refs[key], r.Ref.DisplayName()))
			}
		}
		if len(legend) > 0 {
			text += "\n\n" + strings.Join(legend, "\n")
		}
		return core.Answer{Text: redact.Text(text, 16000), Resources: result, Evidence: evidence}
	}
	read := func(tool string, args map[string]any) core.ToolResult {
		raw, _ := json.Marshal(args)
		return i.execute(ctx, tool, string(raw))
	}
	onEvent("Reading the selected Service identity and selector.")
	svc := read("get_resource", map[string]any{"kind": "services", "namespace": selected.Namespace, "name": selected.Name})
	if len(svc.Errors) > 0 {
		add(svc)
		return finish("The selected Service could not be verified; no routing comparison was made.")
	}
	if len(svc.Resources) != 1 {
		return core.Answer{Text: "The selected Service read did not return one exact identity; no routing comparison was made.", Evidence: []core.ToolResult{svc}}
	}
	actual := svc.Resources[0].Ref
	if actual.Kind != selected.Kind || actual.Namespace != selected.Namespace || actual.Name != selected.Name || actual.UID == "" || (selected.UID != "" && actual.UID != selected.UID) {
		return core.Answer{Text: "The selected Service identity changed or could not be verified. Refresh and select it again; no routing comparison was made.", Evidence: []core.ToolResult{svc}}
	}
	add(svc)
	for _, kind := range []string{"pods", "endpointslices"} {
		if ctx.Err() != nil {
			return finish("Investigation cancelled or reached its time budget; the following evidence is incomplete.")
		}
		onEvent("Reading a bounded page of " + kind + " in the selected Service namespace.")
		result := read("list_resources", map[string]any{"kind": kind, "namespace": selected.Namespace, "limit": 30})
		filtered := []core.Resource{}
		for _, r := range result.Resources {
			if r.Ref.Kind == kind && r.Ref.Namespace == selected.Namespace {
				if len(filtered) < 30 {
					filtered = append(filtered, r)
				} else {
					result.Errors = append(result.Errors, "Only the first 30 returned resources were retained.")
					break
				}
			} else {
				result.Errors = append(result.Errors, "An unexpected resource identity was excluded from the routing comparison.")
			}
		}
		result.Resources = filtered
		if result.Data["truncated"] == true || result.Data["limited"] == true {
			result.Errors = append(result.Errors, "The "+kind+" page was limited; resources outside this page were not examined.")
		}
		add(result)
	}
	if ctx.Err() != nil {
		return finish("Investigation cancelled or reached its time budget; the following evidence is incomplete.")
	}
	return finish("")
}
