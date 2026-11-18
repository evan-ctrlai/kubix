package agent

import (
	"context"
	"fmt"
	"kubix/internal/config"
	"kubix/internal/core"
	"strings"
	"testing"
)

type routingBackend struct {
	fakeBackend
	requests  []string
	uid       string
	cancel    context.CancelFunc
	truncated bool
}

func (b *routingBackend) GetResource(ctx context.Context, kind, ns, name string) core.ToolResult {
	b.requests = append(b.requests, kind+":"+ns+":"+name)
	if kind == "pods" {
		return core.ToolResult{Resources: []core.Resource{{Ref: core.ResourceRef{Kind: kind, Namespace: ns, Name: name, UID: "pod-uid"}, Status: "Running", Details: map[string]any{"labels": map[string]string{"app": "other"}}}}}
	}
	return core.ToolResult{Resources: []core.Resource{{Ref: core.ResourceRef{Kind: kind, Namespace: ns, Name: name, UID: b.uid}, Status: "ClusterIP", Details: map[string]any{"selector": map[string]string{"app": "wanted"}}}}}
}
func (b *routingBackend) ListResources(ctx context.Context, kind, ns, query string, limit int) core.ToolResult {
	b.requests = append(b.requests, kind+":"+ns)
	if limit != 30 {
		panic("unbounded request")
	}
	if b.cancel != nil {
		b.cancel()
	}
	if kind == "endpointslices" {
		return core.ErrorResult("list_resources", "Kubernetes denied this read (403)")
	}
	return core.ToolResult{Resources: []core.Resource{{Ref: core.ResourceRef{Kind: "pods", Namespace: ns, Name: "p", UID: "p-uid"}, Status: "Running", Details: map[string]any{"labels": map[string]string{"app": "other"}}}}, Data: map[string]any{"truncated": b.truncated}}
}
func TestSelectedServiceRoutingWithoutModel(t *testing.T) {
	b := &routingBackend{uid: "s-uid", truncated: true}
	i := New(b, config.DefaultModelSettings(), false)
	r := i.Ask(context.Background(), "Why does this service not route to pod p?", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	for _, want := range []string{"No model diagnosis was requested", "does not match captured Pod", "403", "[R1] [R2]", "**[R1]**"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("missing %q: %s", want, r.Text)
		}
	}
	if len(b.requests) != 3 {
		t.Fatal(b.requests)
	}
	if strings.Contains(r.Text, "answer was discarded") {
		t.Fatal(r.Text)
	}
}
func TestSelectedServiceRoutingRejectsIdentityAndScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected core.ResourceRef
		actual   string
		reads    int
	}{{"scope", core.ResourceRef{Kind: "services", Namespace: "other", Name: "s"}, "uid", 0}, {"missing name", core.ResourceRef{Kind: "services", Namespace: "demo"}, "uid", 0}, {"missing namespace", core.ResourceRef{Kind: "services", Name: "s"}, "uid", 0}, {"stale", core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s", UID: "old"}, "new", 1}, {"no returned UID", core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, "", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			b := &routingBackend{uid: tc.actual}
			i := New(b, config.DefaultModelSettings(), false)
			r := i.Ask(context.Background(), "Inspect selected service routing", &tc.selected, nil)
			if len(b.requests) != tc.reads || strings.Contains(r.Text, "excludes that Pod") {
				t.Fatalf("%v %s", b.requests, r.Text)
			}
		})
	}
}
func TestSelectedServiceRoutingCancellationStopsReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := &routingBackend{uid: "uid", cancel: cancel}
	i := New(b, config.DefaultModelSettings(), false)
	r := i.Ask(ctx, "Inspect service routing", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	if len(b.requests) != 2 || !strings.Contains(r.Text, "cancelled") {
		t.Fatalf("%v %s", b.requests, r.Text)
	}
}

type selectorRoutingBackend struct {
	routingBackend
	populations int
	deny        bool
}

func (b *selectorRoutingBackend) ListByLabels(ctx context.Context, kind, ns string, selector map[string]string, limit int) core.ToolResult {
	b.requests = append(b.requests, "selector:"+kind+":"+ns)
	if limit != 30 {
		panic("wrong limit")
	}
	if kind == "endpointslices" {
		if selector["kubernetes.io/service-name"] != "s" {
			panic("missing service selector")
		}
		return core.ErrorResult("list_by_labels", "Kubernetes denied this read (403)")
	}
	if selector["app"] != "wanted" {
		panic("wrong pod selector")
	}
	out := []core.Resource{}
	for j := 0; j < b.populations && j < limit; j++ {
		out = append(out, core.Resource{Ref: core.ResourceRef{Kind: "pods", Namespace: ns, Name: fmt.Sprintf("match-%d", j), UID: fmt.Sprintf("uid-%d", j)}, Status: "Running", Details: map[string]any{"labels": map[string]string{"app": "wanted"}}})
	}
	return core.ToolResult{Resources: out, Data: map[string]any{"truncated": b.populations > limit}}
}
func TestServiceSelectorPageIgnoresUnrelatedWorkloadsAndReportsBounds(t *testing.T) {
	b := &selectorRoutingBackend{routingBackend: routingBackend{uid: "s-uid"}, populations: 45}
	i := New(b, config.DefaultModelSettings(), false)
	r := i.Ask(context.Background(), "Inspect Service routing", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	if len(b.requests) != 3 || !strings.HasPrefix(b.requests[1], "selector:pods") || len(r.Resources) != 31 {
		t.Fatalf("%v resources=%d", b.requests, len(r.Resources))
	}
	if strings.Contains(r.Text, "does not match") || !strings.Contains(r.Text, "page was limited") || !strings.Contains(r.Text, "403") {
		t.Fatal(r.Text)
	}
}
func TestExplicitPodBypassesLargeNamespaceList(t *testing.T) {
	b := &selectorRoutingBackend{routingBackend: routingBackend{uid: "s-uid"}, populations: 1000}
	i := New(b, config.DefaultModelSettings(), false)
	r := i.Ask(context.Background(), "Why does s not route to Ready pod target-beyond-first-page?", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	if len(b.requests) != 3 || b.requests[1] != "pods:demo:target-beyond-first-page" || !strings.Contains(r.Text, "does not match captured Pod target-beyond-first-page") {
		t.Fatalf("%v %s", b.requests, r.Text)
	}
}
func TestNamedPodScopeAndAmbiguityNoReads(t *testing.T) {
	for _, q := range []string{"route to pod other/name", "route to pod first and pod second"} {
		b := &routingBackend{uid: "uid"}
		i := New(b, config.DefaultModelSettings(), false)
		r := i.Ask(context.Background(), q, &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
		if len(b.requests) != 0 || !strings.Contains(r.Text, "no reads") {
			t.Fatalf("%v %s", b.requests, r.Text)
		}
	}
}
func TestGenericFallbackDoesNotAccuseUnrelatedPods(t *testing.T) {
	b := &routingBackend{uid: "uid"}
	i := New(b, config.DefaultModelSettings(), false)
	r := i.Ask(context.Background(), "Inspect routing and pod labels", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	if strings.Contains(r.Text, "excludes that Pod") || !strings.Contains(r.Text, "lacks server-side label filtering") {
		t.Fatal(r.Text)
	}
}

type truncatedLabelBackend struct{ selectorRoutingBackend }

func (b *truncatedLabelBackend) ListByLabels(ctx context.Context, kind, ns string, selector map[string]string, limit int) core.ToolResult {
	r := b.selectorRoutingBackend.ListByLabels(ctx, kind, ns, selector, limit)
	for j := range r.Resources {
		r.Resources[j].Details["labels"] = map[string]string{"other": "shown"}
		r.Resources[j].Details["labels_truncated"] = true
	}
	return r
}
func TestServerSelectedPodRetainedWithTruncatedLabelProjection(t *testing.T) {
	b := &truncatedLabelBackend{selectorRoutingBackend: selectorRoutingBackend{routingBackend: routingBackend{uid: "uid"}, populations: 1}}
	i := New(b, config.DefaultModelSettings(), false)
	r := i.Ask(context.Background(), "Inspect service routing", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	if len(r.Resources) != 2 || strings.Contains(r.Text, "No matching Pods") || strings.Contains(r.Text, "does not match") || !strings.Contains(r.Text, "Captured Service selector") {
		t.Fatal(r.Text)
	}
}

func TestGenericPodInspectionWordsDoNotBecomeNames(t *testing.T) {
	for _, q := range []string{"check pod selector", "check pod readiness and routing", "check pod status and endpoints", "inspect pod identity for routing"} {
		name, err := routingPodTarget(q, "demo")
		if name != "" || err != nil {
			t.Fatalf("%q => %q %v", q, name, err)
		}
	}
}
func TestPodSlashExplicitlyNamesReservedWord(t *testing.T) {
	name, err := routingPodTarget("check routing to pod/selector", "demo")
	if name != "selector" || err != nil {
		t.Fatalf("%q %v", name, err)
	}
}

func TestExplicitServiceIntentNeverOverriddenBySelection(t *testing.T) {
	for _, q := range []string{"Why does service other not route?", "Inspect service/other endpoints", "Inspect svc other selector", "Inspect Service named other routing", "Inspect service othernamespace/s routing"} {
		b := &routingBackend{uid: "uid"}
		i := New(b, config.DefaultModelSettings(), false)
		r := i.Ask(context.Background(), q, &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
		if len(b.requests) != 0 || !strings.Contains(r.Text, "different Service") {
			t.Fatalf("%q: %v %s", q, b.requests, r.Text)
		}
	}
}
func TestServiceIntentAllowsSelectionAndOrdinaryQuestions(t *testing.T) {
	s := core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}
	for _, q := range []string{"Why does this service not route?", "Inspect service routing", "Check service named s selector", "Compare Service demo/s and pod target", "Inspect svc/s endpoints"} {
		if err := checkRoutingServiceIntent(q, s); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
}
func TestPodNamedGrammar(t *testing.T) {
	for _, q := range []string{"route to Pod named target", "route to pod named demo/target", "route to pod/target"} {
		got, err := routingPodTarget(q, "demo")
		if got != "target" || err != nil {
			t.Fatalf("%q => %q %v", q, got, err)
		}
	}
	got, err := routingPodTarget("route to Pod named status", "demo")
	if got != "status" || err != nil {
		t.Fatalf("reserved exact name %q %v", got, err)
	}
}
