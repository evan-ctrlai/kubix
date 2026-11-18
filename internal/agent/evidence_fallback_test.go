package agent

import (
	"context"
	"kubix/internal/config"
	"kubix/internal/core"
	"strings"
	"testing"
)

func TestUncitedServiceAnswerRetainsSelectorEvidenceAndDenial(t *testing.T) {
	b := &fakeBackend{resources: []core.Resource{
		{Ref: core.ResourceRef{Kind: "services", Namespace: "demo", Name: "front", UID: "s"}, Status: "ClusterIP", Details: map[string]any{"selector": map[string]string{"app": "wanted"}}},
		{Ref: core.ResourceRef{Kind: "pods", Namespace: "demo", Name: "worker", UID: "p"}, Status: "Running", Details: map[string]any{"labels": map[string]string{"app": "different"}, "labels_truncated": false}},
	}}
	b.list = func(context.Context) core.ToolResult {
		return core.ErrorResult("list_resources", "Kubernetes denied this read (403); check read-only RBAC for the resource.")
	}
	m := &scriptedModel{messages: []Message{call("get_resource", `{"kind":"services","namespace":"demo","name":"front"}`), call("get_resource", `{"kind":"pods","namespace":"demo","name":"worker"}`), call("list_resources", `{"kind":"endpointslices","namespace":"demo"}`), answer("INVENTED: all endpoints are absent; delete the service")}}
	i := NewWithModel(b, config.DefaultModelSettings(), false, m)
	got := i.Ask(context.Background(), "Compare the service selector with the pod labels", nil, nil)
	for _, want := range []string{"answer was discarded", "does not match captured Pod", "[R1] [R2]", "403", "does not establish that resources or endpoints are absent", "not a complete or atomic"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("missing %q: %s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "INVENTED") || strings.Contains(got.Text, "delete the service") {
		t.Fatal("rejected model content leaked")
	}
	if b.reads != 3 {
		t.Fatalf("unexpected extra reads %d", b.reads)
	}
}
func TestServiceFallbackDoesNotInventMissingOrCrossNamespaceLabels(t *testing.T) {
	svc := core.Resource{Ref: core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, Details: map[string]any{"selector": map[string]string{"app": "a"}}}
	for _, tc := range []struct {
		name, ns  string
		labels    any
		truncated bool
	}{{"missing", "demo", nil, false}, {"truncated", "demo", map[string]string{}, true}, {"cross namespace", "other", map[string]string{"app": "b"}, false}, {"matching", "demo", map[string]string{"app": "a"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			pod := core.Resource{Ref: core.ResourceRef{Kind: "pods", Namespace: tc.ns, Name: "p"}, Details: map[string]any{"labels": tc.labels, "labels_truncated": tc.truncated}}
			got := uncitedEvidenceSummary(map[string]core.Resource{"s": svc, "p": pod}, []string{"s", "p"}, map[string]string{"s": "R1", "p": "R2"}, nil)
			if strings.Contains(got, "excludes that Pod") {
				t.Fatal(got)
			}
		})
	}
}
