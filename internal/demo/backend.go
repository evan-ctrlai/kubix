// Package demo supplies the original, entirely offline synthetic cluster.
package demo

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"

	"kubix/internal/core"
)

//go:embed cluster.json
var clusterJSON []byte

type Backend struct{ resources []core.Resource }

func New() *Backend {
	b := &Backend{}
	if err := json.Unmarshal(clusterJSON, &b.resources); err != nil {
		panic("invalid embedded demo cluster")
	}
	return b
}

func (*Backend) ContextName() string { return "demo-cluster" }
func (*Backend) Namespace() string   { return "demo" }

func clone(r core.Resource) core.Resource {
	data, _ := json.Marshal(r)
	var result core.Resource
	_ = json.Unmarshal(data, &result)
	return result
}

func (b *Backend) ListResources(ctx context.Context, kind, namespace, query string, limit int) core.ToolResult {
	tool := "list_resources"
	if ctx.Err() != nil {
		return core.ErrorResult(tool, "Read cancelled")
	}
	if !core.ValidKind(kind) {
		return core.ErrorResult(tool, "Unsupported resource kind")
	}
	if namespace != "" && namespace != "demo" && kind != "nodes" {
		return core.ErrorResult(tool, "Namespace outside current scope")
	}
	if limit < 1 || limit > 100 {
		return core.ErrorResult(tool, "Limit must be between 1 and 100")
	}
	if len(query) > 200 {
		return core.ErrorResult(tool, "Query must be at most 200 characters")
	}
	rows := []core.Resource{}
	for _, resource := range b.resources {
		if resource.Ref.Kind == kind && strings.Contains(strings.ToLower(resource.Ref.Name+" "+resource.Status), strings.ToLower(query)) {
			rows = append(rows, clone(resource))
		}
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return core.ToolResult{Tool: tool, Resources: rows, Data: map[string]any{"synthetic": true, "truncated": truncated}}
}

func (b *Backend) GetResource(ctx context.Context, kind, namespace, name string) core.ToolResult {
	result := b.ListResources(ctx, kind, namespace, "", 100)
	result.Tool = "get_resource"
	if len(result.Errors) > 0 {
		return result
	}
	for _, r := range result.Resources {
		if r.Ref.Name == name {
			result.Resources = []core.Resource{r}
			return result
		}
	}
	return core.ErrorResult("get_resource", "Resource not found")
}

func (b *Backend) PodLogs(ctx context.Context, namespace, pod, container string, previous bool) core.ToolResult {
	result := b.GetResource(ctx, "pods", namespace, pod)
	result.Tool = "get_pod_logs"
	if len(result.Errors) > 0 {
		return result
	}
	r := result.Resources[0]
	containers, _ := r.Details["containers"].([]any)
	valid := false
	for _, item := range containers {
		c, _ := item.(map[string]any)
		name, _ := c["name"].(string)
		if container == "" && len(containers) == 1 {
			container = name
		}
		if container == name {
			valid = true
		}
	}
	if !valid {
		return core.ErrorResult("get_pod_logs", "Choose a container from the selected pod")
	}
	logs := "2026-11-13T15:30:00Z service healthy; accepting connections"
	if strings.HasPrefix(pod, "checkout-") {
		logs = "2026-11-13T15:30:00Z checkout starting version=2.4.0\n2026-11-13T15:30:01Z FATAL: required setting DATABASE_URL is missing\n2026-11-13T15:30:01Z application exiting with code 1"
	}
	result.Data = map[string]any{"synthetic": true, "container": container, "previous": previous, "logs": logs, "pod_uid": r.Ref.UID, "bounded_sample": true}
	return result
}
