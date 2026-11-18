package demo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubix/internal/core"
)

func TestCrashLoopCaptureFreezesWorkloadAndLogs(t *testing.T) {
	b := New()
	pod := b.ListResources(context.Background(), "pods", "demo", "checkout", 10).Resources[0]
	i := b.CollectCrashLoop(context.Background(), pod.Ref, "", nil)
	if len(i.Observations) != 6 || len(i.Resources) != 4 || i.FinishedAt.Before(i.StartedAt) || i.Target != pod.Ref {
		t.Fatalf("incomplete capture: %+v", i)
	}
	for n, kind := range []string{"pods", "replicasets", "deployments"} {
		if i.Observations[n].Ref.Kind != kind || i.Observations[n].Ref.UID == "" || !json.Valid([]byte(i.Observations[n].Payload)) {
			t.Fatalf("missing workload chain: %+v", i.Observations[n])
		}
	}
	logObservation := i.Observations[4]
	if !strings.Contains(logObservation.Query, "previous=true") || !strings.Contains(logObservation.Query, "container=checkout") || !strings.Contains(logObservation.Payload, "DATABASE_URL is missing") || logObservation.State != "sampled" {
		t.Fatalf("wrong log capture: %+v", logObservation)
	}
	before := i.Observations[0].Payload
	b.resources[0].Details["containers"] = nil
	if i.Observations[0].Payload != before || !strings.Contains(before, "CrashLoopBackOff") {
		t.Fatal("capture changed with demo resource")
	}
	for _, citation := range i.Citations {
		if _, _, ok := i.ResolveCitation(citation.ID); !ok {
			t.Fatalf("unresolvable citation: %+v", citation)
		}
	}
}

func TestCrashLoopCaptureRejectsStaleIdentityAndScope(t *testing.T) {
	for _, target := range []core.ResourceRef{
		{Kind: "pods", Namespace: "other", Name: "checkout-7d9b6-x2k9p"},
		{Kind: "pods", Namespace: "demo", Name: "checkout-7d9b6-x2k9p", UID: "replaced"},
		{Kind: "secrets", Namespace: "demo", Name: "secret"},
	} {
		i := New().CollectCrashLoop(context.Background(), target, "", nil)
		if len(i.Resources) != 0 || len(i.Observations) != 1 || i.Observations[0].State != "unavailable" {
			t.Fatalf("unsafe target captured: %+v", i)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	i := New().CollectCrashLoop(ctx, core.ResourceRef{Kind: "pods", Name: "checkout-7d9b6-x2k9p"}, "", nil)
	if len(i.Resources) != 0 || len(i.Observations[0].Errors) == 0 {
		t.Fatal("ignored cancellation")
	}
}

func TestCrashLoopCaptureUsesUIDEventsAndContainerChoice(t *testing.T) {
	b := New()
	pod := b.resources[0]
	containers := pod.Details["containers"].([]any)
	pod.Details["containers"] = append(containers, map[string]any{"name": "sidecar", "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}})
	for n := range b.resources {
		if b.resources[n].Ref.Kind == "events" {
			b.resources[n].Details["involved_object"].(map[string]any)["uid"] = "another-pod"
		}
	}
	i := b.CollectCrashLoop(context.Background(), pod.Ref, "", nil)
	if !strings.Contains(i.Observations[3].Payload, `"matched_count":0`) || i.Observations[4].State != "unavailable" || i.Observations[5].State != "unavailable" {
		t.Fatalf("guessed event identity or container: %+v", i)
	}
	i = b.CollectCrashLoop(context.Background(), pod.Ref, "checkout", nil)
	if i.Observations[4].State != "sampled" {
		t.Fatal("explicit container choice was ignored")
	}
}
