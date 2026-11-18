package demo

import (
	"context"
	"strings"
	"testing"
)

func TestFixturesAndLogs(t *testing.T) {
	b := New()
	ctx := context.Background()
	result := b.ListResources(ctx, "pods", "", "", 50)
	if len(result.Resources) != 3 || result.Resources[0].Status != "CrashLoopBackOff" {
		t.Fatalf("bad pod fixtures: %+v", result)
	}
	pod := result.Resources[0]
	logs := b.PodLogs(ctx, "demo", pod.Ref.Name, "", true)
	if !strings.Contains(logs.Data["logs"].(string), "DATABASE_URL is missing") || logs.Resources[0].Ref.UID != pod.Ref.UID {
		t.Fatalf("bad logs: %+v", logs)
	}
	if logs.Data["previous"] != true {
		t.Fatal("previous flag lost")
	}
}

func TestScopeCancellationAndCopies(t *testing.T) {
	b := New()
	ctx := context.Background()
	if len(b.ListResources(ctx, "pods", "other", "", 50).Errors) == 0 {
		t.Fatal("escaped scope")
	}
	if len(b.ListResources(ctx, "secrets", "", "", 50).Errors) == 0 {
		t.Fatal("unexpected kind allowed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if len(b.ListResources(cancelled, "pods", "", "", 50).Errors) == 0 {
		t.Fatal("ignored cancellation")
	}
	a := b.ListResources(ctx, "pods", "", "", 50)
	a.Resources[0].Details["name"] = "modified"
	if b.ListResources(ctx, "pods", "", "", 50).Resources[0].Details["name"] != nil {
		t.Fatal("fixture mutation leaked")
	}
}
