package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"kubix/internal/core"
)

func TestInvestigationEncodingSharesMetadataAndPreservesCapture(t *testing.T) {
	b := captureFixture()
	changed := b.inv.AddObservation(core.Observation{Title: "Identity changed", State: "changed", Query: "changed-source-query", Errors: []string{"Pod UID changed before this read."}})
	for n := range b.inv.Observations {
		b.inv.Observations[n].Query = fmt.Sprintf("unique-source-query-%d", n)
		b.inv.Observations[n].ResourceVersion = fmt.Sprint(n + 100)
	}
	addFocusedCitations(b.inv)
	before, _ := json.Marshal(b.inv)
	encoded, visible := encodeInvestigation(b.inv)
	var payload struct {
		Sources  []core.Observation `json:"sources"`
		Evidence []map[string]any   `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sources) != len(b.inv.Observations) || len(payload.Evidence) != len(visible) {
		t.Fatal("source or transmitted target count changed")
	}
	for n, source := range payload.Sources {
		original := b.inv.Observations[n]
		if source.ID != original.ID || source.Ref != original.Ref || source.State != original.State || source.Source != original.Source || source.Format != original.Format || source.Title != original.Title || source.Query != original.Query || source.ResourceVersion != original.ResourceVersion || !source.CapturedAt.Equal(original.CapturedAt) || !reflect.DeepEqual(source.Errors, original.Errors) || !reflect.DeepEqual(source.Notes, original.Notes) || source.Payload != "" {
			t.Fatalf("source metadata changed or repeated its full payload: %s", source.ID)
		}
		if strings.Count(encoded, original.Query) != 1 {
			t.Fatalf("query metadata repeated for %s", source.ID)
		}
	}
	if visible["E1"] || visible["E2"] || visible["E3"] || visible[changed.ID] {
		t.Fatal("redundant broad source or unusable source exposed as a citation")
	}
	if !strings.Contains(encoded, "RBAC denied") || !strings.Contains(encoded, "Pod UID changed") || !strings.Contains(encoded, "last 100 lines") {
		t.Fatal("collection gaps or sample limits were lost")
	}
	if len(payload.Evidence) == 0 || payload.Evidence[0]["line_start"] == nil {
		t.Fatal("log line evidence lost priority")
	}
	for _, entry := range payload.Evidence {
		id, _ := entry["id"].(string)
		o, c, ok := b.inv.ResolveCitation(id)
		if !ok || !visible[id] || entry["observation_id"] != o.ID || entry["payload"] != citationPayload(o, c) {
			t.Fatalf("incomplete or unresolvable target %q", id)
		}
		for _, metadata := range []string{"source", "ref", "captured_at", "state", "query", "errors", "notes"} {
			if _, exists := entry[metadata]; exists {
				t.Fatalf("%s metadata repeated in %s", metadata, id)
			}
		}
	}
	after, _ := json.Marshal(b.inv)
	if string(before) != string(after) {
		t.Fatal("inference compaction mutated the full capture or citations")
	}
}

func TestInvestigationEncodingRetainsWholeTargetWithoutFocusedTargets(t *testing.T) {
	b := captureFixture()
	whole := b.inv.AddObservation(core.Observation{Title: "Other captured detail", State: "captured", Format: "json", Payload: `{"other":{"value":"exact full capture"}}`})
	addFocusedCitations(b.inv)
	encoded, visible := encodeInvestigation(b.inv)
	var payload struct {
		Evidence []struct {
			ID            string `json:"id"`
			ObservationID string `json:"observation_id"`
			Payload       string `json:"payload"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	for _, entry := range payload.Evidence {
		if entry.ID == whole.ID {
			o, c, ok := b.inv.ResolveCitation(entry.ID)
			if !ok || !visible[entry.ID] || entry.ObservationID != o.ID || c.JSONPointer != "" || c.LineStart != 0 || entry.Payload != o.Payload {
				t.Fatal("whole-target fallback did not preserve the complete capture")
			}
			return
		}
	}
	t.Fatal("source without focused targets lost its whole-target fallback")
}

func TestInvestigationEncodingBudgetNeverExposesPartialTarget(t *testing.T) {
	b := captureFixture()
	for range 12 {
		b.inv.AddObservation(core.Observation{Title: "Long captured log", State: "sampled", Format: "log", Payload: "ERROR " + strings.Repeat("<", 900)})
	}
	addFocusedCitations(b.inv)
	encoded, visible := encodeInvestigation(b.inv)
	var payload struct {
		Sources  []core.Observation `json:"sources"`
		Evidence []struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
		} `json:"evidence"`
	}
	if len(encoded) > MaxToolChars || json.Unmarshal([]byte(encoded), &payload) != nil || len(payload.Sources) != len(b.inv.Observations) {
		t.Fatal("budget overflow or source metadata lost")
	}
	actual := map[string]bool{}
	for _, entry := range payload.Evidence {
		o, c, ok := b.inv.ResolveCitation(entry.ID)
		if !ok || entry.Payload != citationPayload(o, c) {
			t.Fatal("truncated citation payload reached the model")
		}
		actual[entry.ID] = true
	}
	if !reflect.DeepEqual(actual, visible) {
		t.Fatal("visibility differs from the exact transmitted targets")
	}
	omittedFocused := false
	for _, citation := range b.inv.Citations {
		if !visible[citation.ID] {
			omittedFocused = omittedFocused || citation.JSONPointer != "" || citation.LineStart > 0
			raw := `{"observations":[{"text":"Unsent target","citations":["` + citation.ID + `"]}]}`
			if _, err := renderInvestigationSummary(raw, b.inv, visible); err == nil {
				t.Fatalf("renderer accepted omitted target %s", citation.ID)
			}
		}
	}
	if !omittedFocused {
		t.Fatal("fixture did not fill the budget enough to omit a focused target")
	}
}

func TestInvestigationEncodingMetadataOverflowFailsClosed(t *testing.T) {
	b := captureFixture()
	b.inv.Observations[2].Errors = []string{strings.Repeat("x", MaxToolChars)}
	addFocusedCitations(b.inv)
	before, _ := json.Marshal(b.inv)
	encoded, visible := encodeInvestigation(b.inv)
	if len(encoded) > MaxToolChars || !json.Valid([]byte(encoded)) || len(visible) != 0 || !strings.Contains(encoded, "Capture metadata exceeds the inference budget") {
		t.Fatal("over-budget source metadata did not fail closed")
	}
	after, _ := json.Marshal(b.inv)
	if string(before) != string(after) {
		t.Fatal("over-budget encoding modified the UI capture")
	}
}
