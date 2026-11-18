package agent

import (
	"context"
	"encoding/json"
	"kubix/internal/config"
	"kubix/internal/core"
	"strings"
	"testing"
)

type neverSummary struct{ calls int }

func (m *neverSummary) Complete(context.Context, []Message, []ToolDefinition) (Message, error) {
	m.calls++
	return Message{Content: "unsafe"}, nil
}
func TestConservativeTemporalCaptureGuards(t *testing.T) {
	for _, scenario := range []string{"mixed", "healthy"} {
		t.Run(scenario, func(t *testing.T) {
			b := captureFixture()
			b.inv.Observations = nil
			b.inv.Citations = nil
			ref := b.inv.Target
			r := core.Resource{Ref: ref, Status: "Running", Details: map[string]any{"containers": []any{map[string]any{"name": "app", "ready": true, "restart_count": 0}}}}
			if scenario == "mixed" {
				r.Status = "Failed"
			}
			raw, _ := json.Marshal(r)
			b.inv.AddObservation(core.Observation{Ref: ref, Source: "kubernetes/pods", Format: "json", State: "captured", Payload: string(raw)})
			b.inv.AddObservation(core.Observation{Ref: ref, Source: "kubernetes/pods/log", Title: "Previous logs - app", Format: "log", State: "unavailable"})
			log := "healthy replacement running"
			if scenario == "mixed" {
				log = "STALE (cached): previous diagnosis claimed DATABASE_URL was missing\nFATAL (current): release signature verification failed"
			}
			b.inv.AddObservation(core.Observation{Ref: ref, Source: "kubernetes/pods/log", Title: "Current logs - app", Format: "log", State: "sampled", Payload: log})
			m := &neverSummary{}
			a := NewWithModel(b, config.DefaultModelSettings(), false, m)
			answer := a.InvestigateCrashLoop(context.Background(), ref, "app", nil)
			if m.calls != 0 || strings.Contains(answer.Text, "Possible explanations") || answer.Investigation == nil {
				t.Fatalf("unsafe summary: calls=%d %s", m.calls, answer.Text)
			}
			if !strings.Contains(answer.Text, "Current logs") {
				t.Fatal("lost source provenance")
			}
			if scenario == "healthy" && !strings.Contains(answer.Text, "cannot be established") {
				t.Fatal("missing historical uncertainty")
			}
		})
	}
}
func TestGuardDoesNotSuppressOrdinaryCrashLoop(t *testing.T) {
	if got := conservativeCaptureReason(captureFixture().inv); got != "" {
		t.Fatal(got)
	}
}
func TestGuardRequiresExplicitHealthyEvidence(t *testing.T) {
	b := captureFixture()
	for _, o := range []core.Observation{{Source: "kubernetes/pods/log", Title: "Previous logs", State: "unavailable"}, {Source: "kubernetes/pods/log", Title: "Current logs", State: "sampled", Format: "log", Payload: "cached configuration loaded"}} {
		b.inv.AddObservation(o)
	}
	if got := conservativeCaptureReason(b.inv); got != "" {
		t.Fatal(got)
	}
}

func TestCurrentTerminationHasDistinctCitations(t *testing.T) {
	b := captureFixture()
	b.inv.Observations = nil
	b.inv.Citations = nil
	r := core.Resource{Ref: b.inv.Target, Status: "Failed", Details: map[string]any{"containers": []any{map[string]any{"name": "app", "restart_count": 0, "state": map[string]any{"terminated": map[string]any{"reason": "Error", "exit_code": 42}}, "last_state": map[string]any{"terminated": map[string]any{"reason": "OldError", "exit_code": 1}}}}}}
	data, _ := json.Marshal(r)
	b.inv.AddObservation(core.Observation{Ref: r.Ref, Source: "kubernetes/pods", Format: "json", State: "captured", Payload: string(data)})
	addFocusedCitations(b.inv)
	text := deterministicInvestigation(b.inv)
	if !strings.Contains(text, "current termination: Error") || !strings.Contains(text, "exit 42") || strings.Contains(text, "OldError") {
		t.Fatal(text)
	}
	found := false
	for _, c := range b.inv.Citations {
		if c.JSONPointer == "/details/containers/0/state/terminated/exit_code" {
			found = true
			if _, _, ok := b.inv.ResolveCitation(c.ID); !ok {
				t.Fatal("invalid current exit citation")
			}
		}
	}
	if !found {
		t.Fatal("no current exit citation")
	}
}
