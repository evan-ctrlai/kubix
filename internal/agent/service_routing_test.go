package agent

import (
	"context"
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
	r := i.Ask(context.Background(), "Why does this service not route?", &core.ResourceRef{Kind: "services", Namespace: "demo", Name: "s"}, nil)
	for _, want := range []string{"No model diagnosis was requested", "does not match captured Pod", "403", "page was limited", "[R1] [R2]", "**[R1]**"} {
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
