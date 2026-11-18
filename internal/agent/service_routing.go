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
var namedRoutingPod = regexp.MustCompile("(?i)\\bpod(?:/|\\s+(?:named\\s+)?)[`\"]?([a-z0-9][a-z0-9.-]*(?:/[a-z0-9][a-z0-9.-]*)?)")

func serviceRoutingQuestion(question string, selected *core.ResourceRef) bool {
	return selected != nil && selected.Kind == "services" && routingQuestion.MatchString(question)
}
func routingPodTarget(question, namespace string) (string, error) {
	names := map[string]bool{}
	stop := map[string]bool{"labels": true, "label": true, "is": true, "with": true, "that": true, "this": true, "selected": true, "in": true, "and": true, "name": true, "for": true, "to": true, "logs": true, "ports": true, "selector": true, "selectors": true, "readiness": true, "status": true, "state": true, "states": true, "health": true, "identity": true, "history": true, "details": true, "conditions": true, "restarts": true, "restart": true, "port": true, "log": true, "evidence": true, "configuration": true, "config": true, "routing": true, "endpoints": true}
	for _, m := range namedRoutingPod.FindAllStringSubmatch(question, -1) {
		name := strings.TrimRight(m[1], ".")
		if stop[strings.ToLower(name)] && !routingNameExplicit(m[0]) {
			continue
		}
		if parts := strings.Split(name, "/"); len(parts) == 2 {
			if parts[0] != namespace {
				return "", fmt.Errorf("Named Pod is outside the selected Service namespace")
			}
			name = parts[1]
		}
		names[name] = true
	}
	if len(names) > 1 {
		return "", fmt.Errorf("Name one target Pod for a bounded Service-to-Pod comparison")
	}
	for name := range names {
		return name, nil
	}
	return "", nil
}

// Three reads maximum: exact Service, a named Pod OR a server-selector Pod page,
// and a Service-specific EndpointSlice page. No mutation or model is required.
func (i *Investigator) investigateServiceRouting(ctx context.Context, question string, selected core.ResourceRef, onEvent func(string)) core.Answer {
	if selected.Kind != "services" || selected.Name == "" || selected.Namespace == "" || len(selected.Name) > 253 || len(selected.Namespace) > 63 {
		return core.Answer{Text: "Select a Service with an explicit name and namespace."}
	}
	if scope := i.backend.Namespace(); scope != "" && selected.Namespace != scope {
		return core.Answer{Text: "The selected Service is outside the configured namespace scope."}
	}
	if err := checkRoutingServiceIntent(question, selected); err != nil {
		return core.Answer{Text: err.Error() + "; no reads were made."}
	}
	target, err := routingPodTarget(question, selected.Namespace)
	if err != nil {
		return core.Answer{Text: err.Error() + "; no reads were made."}
	}
	if ctx.Err() != nil {
		return core.Answer{Text: "Investigation cancelled or reached its time budget."}
	}
	resources := map[string]core.Resource{}
	order := []string{}
	refs := map[string]string{}
	evidence := []core.ToolResult{}
	add := func(r core.ToolResult) {
		evidence = append(evidence, r)
		for _, v := range r.Resources {
			key := v.Ref.Key()
			if _, ok := refs[key]; !ok {
				refs[key] = fmt.Sprintf("R%d", len(refs)+1)
				order = append(order, key)
			}
			resources[key] = v
		}
	}
	scopeNote := ""
	finish := func(note string) core.Answer {
		text := collectedEvidenceSummary(resources, order, refs, evidence, target != "")
		text = strings.Replace(text, "The model did not cite its retrieved resources. Its answer was discarded.", "Read-only routing inspection of the selected Service. No model diagnosis was requested.", 1)
		if scopeNote != "" {
			text += "\n\n" + scopeNote
		}
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
	selector, hasSelector := stringFields(svc.Resources[0].Details["selector"])
	if hasSelector {
		encoded, _ := json.Marshal(selector)
		scopeNote = fmt.Sprintf("Captured Service selector: %s. [%s]", string(encoded), refs[actual.Key()])
	}
	for _, kind := range []string{"pods", "endpointslices"} {
		if ctx.Err() != nil {
			return finish("Investigation cancelled or reached its time budget; the following evidence is incomplete.")
		}
		var result core.ToolResult
		serverFiltered := false
		if kind == "pods" && target != "" {
			onEvent("Reading the explicitly named target Pod.")
			result = read("get_resource", map[string]any{"kind": "pods", "namespace": selected.Namespace, "name": target})
			if len(result.Errors) == 0 && (len(result.Resources) != 1 || result.Resources[0].Ref.Name != target || result.Resources[0].Ref.UID == "") {
				result = core.ErrorResult("get_resource", "Named Pod identity could not be verified; no Pod comparison was made.")
			}
		} else {
			filter := selector
			if kind == "endpointslices" {
				filter = map[string]string{"kubernetes.io/service-name": selected.Name}
			}
			if kind == "pods" && (!hasSelector || len(selector) == 0) {
				add(core.ErrorResult("routing_scope", "Service has no captured selector; no namespace-wide Pod search was performed. A selectorless Service may use manually managed endpoints."))
				continue
			}
			onEvent("Reading one Service-filtered page of " + kind + ".")
			if backend, ok := i.backend.(core.LabelSelectorReader); ok {
				result = readRoutingLabels(ctx, backend, kind, selected.Namespace, filter)
				serverFiltered = true
			} else {
				result = read("list_resources", map[string]any{"kind": kind, "namespace": selected.Namespace, "limit": 30})
				result.Errors = append(result.Errors, "Backend lacks server-side label filtering; only matching resources in the first namespace page can be shown. Absence is not established.")
			}
		}
		filtered := []core.Resource{}
		for _, r := range result.Resources {
			if r.Ref.Kind != kind || r.Ref.Namespace != selected.Namespace || r.Ref.UID == "" {
				result.Errors = append(result.Errors, "An unexpected or incomplete resource identity was excluded from the routing comparison.")
				continue
			}
			if kind == "pods" && target == "" && !serverFiltered {
				if r.Details["labels_truncated"] == true {
					result.Errors = append(result.Errors, "Pod labels were truncated; local selector matching is incomplete.")
				}
				podLabels, ok := stringFields(r.Details["labels"])
				if !ok || !matchesRoutingLabels(selector, podLabels) {
					continue
				}
			}
			if kind == "endpointslices" && r.Details["service_name"] != selected.Name {
				continue
			}
			if len(filtered) >= 30 {
				result.Errors = append(result.Errors, "Only the first 30 matching returned resources were retained.")
				break
			}
			filtered = append(filtered, r)
		}
		result.Resources = filtered
		if result.Data["truncated"] == true || result.Data["limited"] == true {
			result.Errors = append(result.Errors, "The "+kind+" page was limited; resources outside this page were not examined.")
		}
		if kind == "pods" && target == "" && len(filtered) == 0 && len(result.Errors) == 0 {
			result.Errors = append(result.Errors, "No matching Pods were observed in this bounded selector query; this is not a connectivity test or a historical conclusion.")
		}
		add(result)
	}
	if ctx.Err() != nil {
		return finish("Investigation cancelled or reached its time budget; the following evidence is incomplete.")
	}
	return finish("")
}
func matchesRoutingLabels(selector, values map[string]string) bool {
	for k, v := range selector {
		if actual, ok := values[k]; !ok || actual != v {
			return false
		}
	}
	return len(selector) > 0
}
func readRoutingLabels(ctx context.Context, b core.LabelSelectorReader, kind, namespace string, selector map[string]string) (result core.ToolResult) {
	result.Tool = "list_by_labels"
	defer func() {
		if recover() != nil {
			result = core.ErrorResult("list_by_labels", "Kubernetes label read failed; check access and cluster connectivity")
		}
	}()
	if ctx.Err() != nil {
		return core.ErrorResult("list_by_labels", "Investigation cancelled or timed out")
	}
	result = b.ListByLabels(ctx, kind, namespace, selector, 30)
	for n := range result.Resources {
		r := &result.Resources[n]
		r.Status = redact.Text(r.Status, 16000)
		r.Summary = redact.Text(r.Summary, 16000)
		r.Details, _ = redact.Value(r.Details, 16000).(map[string]any)
	}
	result.Data, _ = redact.Value(result.Data, 16000).(map[string]any)
	for n := range result.Errors {
		result.Errors[n] = redact.Text(result.Errors[n], 16000)
	}
	return result
}

var namedRoutingService = regexp.MustCompile("(?i)\\b(?:service|svc)(?:/|\\s+(?:named\\s+)?)[`\"]?([a-z0-9][a-z0-9.-]*(?:/[a-z0-9][a-z0-9.-]*)?)")

func routingNameExplicit(mention string) bool {
	return strings.Contains(mention, "/") || strings.Contains(strings.ToLower(mention), " named ") || strings.ContainsAny(mention, "`\"")
}
func checkRoutingServiceIntent(question string, selected core.ResourceRef) error {
	generic := map[string]bool{"routing": true, "selector": true, "selectors": true, "endpoints": true, "endpoint": true, "name": true, "named": true, "is": true, "not": true, "does": true, "doesn": true, "do": true, "has": true, "have": true, "cannot": true, "can": true, "won": true, "will": true, "would": true, "should": true, "which": true, "and": true, "to": true, "in": true, "with": true, "for": true, "ports": true, "port": true, "status": true, "inspection": true, "traffic": true, "connectivity": true, "or": true, "an": true, "a": true, "the": true, "this": true, "selected": true, "that": true, "readiness": true, "health": true, "details": true, "configuration": true, "config": true, "labels": true}
	for _, m := range namedRoutingService.FindAllStringSubmatch(question, -1) {
		name := strings.TrimRight(m[1], ".")
		if generic[strings.ToLower(name)] && !routingNameExplicit(m[0]) {
			continue
		}
		ns := selected.Namespace
		if parts := strings.Split(name, "/"); len(parts) == 2 {
			ns, name = parts[0], parts[1]
		}
		if ns != selected.Namespace || name != selected.Name {
			return fmt.Errorf("The question explicitly names a different Service than the selection. Select the intended Service or narrow the question")
		}
	}
	return nil
}
