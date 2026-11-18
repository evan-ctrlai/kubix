package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"kubix/internal/core"
	"kubix/internal/redact"
)

const MaxToolChars = 24000
const MaxContextChars = 96000

var allowedKinds = map[string]bool{"pods": true, "deployments": true, "services": true, "endpointslices": true, "events": true, "nodes": true}

func toolDefinitions() []ToolDefinition {
	stringType := func() map[string]any { return map[string]any{"type": "string"} }
	namespace := map[string]any{"type": []string{"string", "null"}, "maxLength": 63}
	kind := map[string]any{"type": "string", "enum": []string{"pods", "deployments", "services", "endpointslices", "events", "nodes"}}
	definitions := []struct {
		name, description string
		properties        map[string]any
		required          []string
	}{
		{"list_resources", "List one bounded page in the current namespace scope; query filters names/status. Pod labels, Service selectors/ports and EndpointSlice summaries are included for routing diagnosis. Check truncation.", map[string]any{"kind": kind, "namespace": namespace, "query": map[string]any{"type": "string", "maxLength": 200}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, []string{"kind"}},
		{"get_resource", "Read sanitized details of one resource. Secrets and environment values are excluded.", map[string]any{"kind": kind, "namespace": namespace, "name": stringType()}, []string{"kind", "name"}},
		{"get_pod_logs", "Read up to 100 lines / 16 KB within 15 minutes; previous reads the previous container instance.", map[string]any{"namespace": stringType(), "pod": stringType(), "container": map[string]any{"type": []string{"string", "null"}}, "previous": map[string]any{"type": "boolean"}}, []string{"namespace", "pod"}},
		{"investigate_pod", "Capture a bounded, read-only CrashLoop investigation of one pod, including status, related events and current/previous logs. Returns a report grounded in immutable captured evidence.", map[string]any{"namespace": stringType(), "pod": stringType(), "container": map[string]any{"type": []string{"string", "null"}}}, []string{"namespace", "pod"}},
	}
	result := make([]ToolDefinition, 0, len(definitions))
	for _, d := range definitions {
		result = append(result, ToolDefinition{Type: "function", Function: map[string]any{"name": d.name, "description": d.description, "parameters": map[string]any{"type": "object", "properties": d.properties, "required": d.required, "additionalProperties": false}}})
	}
	return result
}

func allowedTool(name string) bool {
	return name == "list_resources" || name == "get_resource" || name == "get_pod_logs" || name == "investigate_pod"
}

func (i *Investigator) execute(ctx context.Context, name, arguments string) (result core.ToolResult) {
	result.Tool = name
	if !allowedTool(name) {
		return core.ToolResult{Tool: "rejected", Errors: []string{"Unknown tool; only list_resources, get_resource and get_pod_logs are allowed"}}
	}
	defer func() {
		if recover() != nil {
			result = core.ToolResult{Tool: name, Errors: []string{"Kubernetes read failed; check access and cluster connectivity"}}
		}
	}()
	args, err := decodeArguments(name, arguments)
	if err != nil {
		return core.ToolResult{Tool: name, Errors: []string{"Invalid tool arguments"}}
	}
	if ctx.Err() != nil {
		return core.ToolResult{Tool: name, Errors: []string{"Investigation cancelled or timed out"}}
	}
	if args.kind != "nodes" && i.backend.Namespace() != "" && args.namespace != "" && args.namespace != i.backend.Namespace() {
		return core.ToolResult{Tool: name, Errors: []string{"Namespace outside the configured scope"}}
	}
	switch name {
	case "list_resources":
		result = i.backend.ListResources(ctx, args.kind, args.namespace, args.query, args.limit)
	case "get_resource":
		result = i.backend.GetResource(ctx, args.kind, args.namespace, args.name)
	case "get_pod_logs":
		result = i.backend.PodLogs(ctx, args.namespace, args.name, args.container, args.previous)
	case "investigate_pod":
		return core.ToolResult{Tool: name, Errors: []string{"Investigation capture must run through the investigator workflow"}}
	}
	result.Tool = name
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

type arguments struct {
	kind, namespace, query, name, container string
	limit                                   int
	previous                                bool
}

func decodeArguments(name, raw string) (arguments, error) {
	a := arguments{limit: 30}
	var values map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(raw))
	if len(raw) > 8192 || decoder.Decode(&values) != nil || values == nil {
		return a, fmt.Errorf("invalid JSON arguments")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return a, fmt.Errorf("trailing JSON")
	}
	allowed := map[string]bool{"kind": true, "namespace": true}
	switch name {
	case "list_resources":
		allowed["query"], allowed["limit"] = true, true
	case "get_resource":
		allowed["name"] = true
	case "get_pod_logs":
		allowed = map[string]bool{"namespace": true, "pod": true, "container": true, "previous": true}
	case "investigate_pod":
		allowed = map[string]bool{"namespace": true, "pod": true, "container": true}
	}
	for key := range values {
		if !allowed[key] {
			return a, fmt.Errorf("unknown field")
		}
	}
	readString := func(key string, required, nullable bool, max int) (string, error) {
		value, present := values[key]
		if !present {
			if required {
				return "", fmt.Errorf("missing field")
			}
			return "", nil
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if nullable {
				return "", nil
			}
			return "", fmt.Errorf("null field")
		}
		var result string
		if json.Unmarshal(value, &result) != nil || utf8.RuneCountInString(result) > max || (required && result == "") {
			return "", fmt.Errorf("invalid string")
		}
		return result, nil
	}
	var readErr error
	if name != "get_pod_logs" && name != "investigate_pod" {
		a.kind, readErr = readString("kind", true, false, 20)
		if readErr != nil || !allowedKinds[a.kind] {
			return a, fmt.Errorf("invalid kind")
		}
	}
	var err error
	podRead := name == "get_pod_logs" || name == "investigate_pod"
	a.namespace, err = readString("namespace", podRead, !podRead, 63)
	if err != nil {
		return a, err
	}
	if name == "get_resource" {
		a.name, err = readString("name", true, false, 253)
	} else if podRead {
		a.name, err = readString("pod", true, false, 253)
		if err == nil {
			a.container, err = readString("container", false, true, 63)
		}
		if value, present := values["previous"]; present && (bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &a.previous) != nil) {
			err = fmt.Errorf("invalid boolean")
		}
	} else {
		a.query, err = readString("query", false, false, 200)
		if value, present := values["limit"]; present && (bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &a.limit) != nil || a.limit < 1 || a.limit > 100) {
			err = fmt.Errorf("invalid limit")
		}
	}
	return a, err
}

func encodeEvidence(result core.ToolResult, refs map[string]string) (string, []string) {
	rows := make([]map[string]any, 0, len(result.Resources))
	for _, resource := range result.Resources {
		row := map[string]any{"id": refs[resource.Ref.Key()], "ref": resource.Ref, "status": resource.Status, "summary": resource.Summary}
		if result.Tool != "list_resources" {
			row["details"] = resource.Details
		} else {
			// Keep list responses compact while exposing the fields needed to
			// compare Service selectors with candidate Pods and EndpointSlices.
			switch resource.Ref.Kind {
			case "pods":
				if labels, ok := resource.Details["labels"]; ok {
					row["details"] = map[string]any{"labels": labels, "labels_truncated": resource.Details["labels_truncated"]}
				}
			case "services":
				row["details"] = map[string]any{"selector": resource.Details["selector"], "ports": resource.Details["ports"]}
			case "endpointslices":
				row["details"] = map[string]any{"service_name": resource.Details["service_name"],
					"endpoint_count": resource.Details["endpoint_count"], "ready_count_sampled": resource.Details["ready_count_sampled"],
					"ready_condition_count_sampled": resource.Details["ready_condition_count_sampled"],
					"endpoints_truncated":           resource.Details["endpoints_truncated"]}
			}
		}
		rows = append(rows, row)
	}
	payload := map[string]any{"resources": rows, "data": result.Data, "errors": result.Errors}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"resources":[],"errors":["Evidence could not be encoded"]}`, nil
	}
	if len(encoded) > MaxToolChars {
		payload["truncated"] = true
		payload["notice"] = "Evidence exceeds the response budget. Request one resource or narrower logs."
		for _, row := range rows {
			b, _ := json.Marshal(row)
			if len(b) > 6000 {
				delete(row, "details")
				row["details_omitted"] = true
			}
		}
		data, _ := json.Marshal(result.Data)
		if len(data) > 18000 {
			payload["data"] = map[string]any{"sample": redact.Text(string(data), 12000), "truncated": true}
		}
		encoded, _ = json.Marshal(payload)
		for len(rows) > 0 && len(encoded) > MaxToolChars {
			rows = rows[:len(rows)-1]
			payload["resources"] = rows
			encoded, _ = json.Marshal(payload)
		}
		if len(encoded) > MaxToolChars {
			errors := []string{}
			for _, e := range result.Errors[:min(len(result.Errors), 10)] {
				errors = append(errors, redact.Text(e, 500))
			}
			payload["errors"] = errors
			encoded, _ = json.Marshal(payload)
		}
		if len(encoded) > MaxToolChars {
			payload["data"] = map[string]any{"omitted": true, "reason": "Serialized evidence exceeds budget"}
			encoded, _ = json.Marshal(payload)
		}
		// JSON escaping can expand even short Unicode/HTML-rich error strings.
		if len(encoded) > MaxToolChars {
			payload["errors"] = []string{"Additional evidence errors omitted to fit the response budget"}
			encoded, _ = json.Marshal(payload)
		}
	}
	visible := make([]string, 0, len(rows))
	for _, row := range rows {
		visible = append(visible, row["id"].(string))
	}
	return string(encoded), visible
}
