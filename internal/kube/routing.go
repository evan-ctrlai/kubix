package kube

import (
	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"kubix/internal/core"
)

// ListByLabels applies server-side equality selectors before bounding the page.
// This avoids losing a Service's Pods behind unrelated namespace workloads.
func (b *Backend) ListByLabels(ctx context.Context, kind, namespace string, selector map[string]string, limit int) core.ToolResult {
	const tool = "list_by_labels"
	if kind != "pods" && kind != "endpointslices" {
		return invalid(tool, "Label lookup supports only Pods and EndpointSlices")
	}
	scope, err := b.scope(kind, namespace)
	if err != nil {
		return invalid(tool, err.Error())
	}
	if scope == "" {
		return invalid(tool, "Label lookup requires an explicit namespace")
	}
	if limit < 1 || limit > maxList || len(selector) == 0 || len(selector) > 50 {
		return invalid(tool, "Label lookup requires 1–50 selector fields and a limit of 1–100")
	}
	for key, value := range selector {
		if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return invalid(tool, "Invalid label selector")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	opts := metav1.ListOptions{LabelSelector: labels.Set(selector).AsSelector().String(), Limit: int64(limit)}
	resources := []core.Resource{}
	continuation := ""
	fetched := 0
	switch kind {
	case "pods":
		list, e := b.client.CoreV1().Pods(scope).List(ctx, opts)
		if e != nil {
			return failure(tool, e)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for j := 0; j < min(len(list.Items), limit); j++ {
			resources = append(resources, podResource(&list.Items[j]))
		}
	case "endpointslices":
		list, e := b.client.DiscoveryV1().EndpointSlices(scope).List(ctx, opts)
		if e != nil {
			return failure(tool, e)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for j := 0; j < min(len(list.Items), limit); j++ {
			resources = append(resources, endpointSliceResource(&list.Items[j]))
		}
	}
	return core.ToolResult{Tool: tool, Resources: resources, Data: map[string]any{"kind": kind, "namespace": scope, "fetched": fetched, "returned": len(resources), "limit": limit, "truncated": continuation != "" || fetched > limit, "limited": continuation != "" || fetched >= limit, "selector_scope": "Server-side equality filter; one bounded page, no pagination"}}
}
