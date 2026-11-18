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

func TestTemporalNarrativeClassificationIsNarrow(t *testing.T) {
	cases := []struct {
		name, payload, query, title string
		want                        bool
	}{
		{"explicit historical diagnosis", "2026-11-18T10:00:00Z HISTORICAL DIAGNOSIS: database connection failed during yesterday's release\n2026-11-18T10:00:01Z FATAL: release signature verification failed", "previous=false", "Current logs", true},
		{"original fixture", "STALE (cached): previous diagnosis claimed DATABASE_URL was missing\nFATAL (current): release signature verification failed", "previous=false", "Current logs", true},
		{"ordinary cache startup", "cached configuration loaded\ncurrent version v2 is now ready", "previous=false", "Current logs", false},
		{"cache failure is current evidence", "cached credentials missing; current request failed", "previous=false", "Current logs", false},
		{"historical annotation alone", "HISTORICAL DIAGNOSIS: connection failed yesterday\ncurrent cache warmed successfully", "previous=false", "Current logs", false},
		{"normal stale cache error", "stale cache entry discarded\nERROR: current request failed", "previous=false", "Current logs", false},
		{"previous source not reclassified", "STALE (cached): previous diagnosis claimed DATABASE_URL was missing\nFATAL (current): release signature verification failed", "previous=true", "Current logs", false},
		{"unknown provenance not guessed", "STALE: connection failed\nFATAL: release signature verification failed", "GET logs", "Current logs", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := core.Observation{Source: "kubernetes/pods/log", Format: "log", State: "sampled", Payload: tc.payload, Query: tc.query, Title: tc.title}
			if got := mixedDiagnosticNarrative(o); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestTemporalGuardDoesNotCombineUnrelatedSources(t *testing.T) {
	b := captureFixture()
	b.inv.Observations = nil
	for _, payload := range []string{"STALE: database connection failed", "FATAL: release signature verification failed"} {
		b.inv.AddObservation(core.Observation{Ref: b.inv.Target, Source: "kubernetes/pods/log", Format: "log", State: "sampled", Query: "previous=false", Payload: payload})
	}
	if reason := conservativeCaptureReason(b.inv); reason != "" {
		t.Fatal(reason)
	}
	other := b.inv.Target
	other.UID = "replacement-uid"
	b.inv.AddObservation(core.Observation{Ref: other, Source: "kubernetes/pods/log", Format: "log", State: "sampled", Query: "previous=false", Payload: "STALE: database connection failed\nFATAL: release signature verification failed"})
	if reason := conservativeCaptureReason(b.inv); reason != "" {
		t.Fatal(reason)
	}
}

func TestEncodedLogsPreserveCollectorStream(t *testing.T) {
	b := captureFixture()
	b.inv.Observations = nil
	b.inv.Citations = nil
	for _, previous := range []string{"true", "false"} {
		b.inv.AddObservation(core.Observation{Ref: b.inv.Target, Source: "kubernetes/pods/log", Format: "log", State: "sampled", Query: "GET pod/log previous=" + previous, Title: "application log", Payload: "FATAL: failure"})
	}
	addFocusedCitations(b.inv)
	payload, _ := encodeInvestigation(b.inv)
	var encoded struct {
		Sources []struct {
			Stream string `json:"log_stream"`
		} `json:"sources"`
		Evidence []struct {
			Stream string `json:"log_stream"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(payload), &encoded); err != nil {
		t.Fatal(err)
	}
	if len(encoded.Sources) != 2 || encoded.Sources[0].Stream != "previous" || encoded.Sources[1].Stream != "current" {
		t.Fatalf("bad provenance: %s", payload)
	}
	seen := map[string]bool{}
	for _, e := range encoded.Evidence {
		seen[e.Stream] = true
	}
	if !seen["previous"] || !seen["current"] {
		t.Fatalf("citation metadata lost stream: %s", payload)
	}
}

func TestIdenticalLogTextRetainsDistinctProvenance(t *testing.T) {
	b := captureFixture()
	b.inv.Observations = nil
	b.inv.Citations = nil
	for _, stream := range []string{"Previous", "Current"} {
		b.inv.AddObservation(core.Observation{Ref: b.inv.Target, Title: stream + " logs", Source: "kubernetes/pods/log", Format: "log", State: "sampled", Payload: "FATAL: common failure"})
	}
	addFocusedCitations(b.inv)
	answer := deterministicInvestigation(b.inv)
	if !strings.Contains(answer, "Previous logs report") || !strings.Contains(answer, "Current logs report") {
		t.Fatalf("identical text lost source identity: %s", answer)
	}
}

func TestBenignCacheNarrativeStillRequestsModelSummary(t *testing.T) {
	b := captureFixture()
	b.inv.AddObservation(core.Observation{Ref: b.inv.Target, Title: "Current logs", Source: "kubernetes/pods/log", Format: "log", State: "sampled", Query: "GET pod/log previous=false", Payload: "cached configuration loaded\nERROR: current request failed"})
	m := &neverSummary{}
	a := NewWithModel(b, config.DefaultModelSettings(), false, m)
	a.InvestigateCrashLoop(context.Background(), b.inv.Target, "app", nil)
	if m.calls != 1 {
		t.Fatalf("benign cache/current wording suppressed model: %d calls", m.calls)
	}
}
