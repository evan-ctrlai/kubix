package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubix/internal/config"
	"kubix/internal/core"
)

func TestInvestigationSchemaExcludesOmittedAndUnusableEvidence(t *testing.T) {
	b := captureFixture()
	changed := b.inv.AddObservation(core.Observation{State: "changed", Payload: "old identity"})
	large := b.inv.AddObservation(core.Observation{State: "captured", Payload: strings.Repeat("x", 7000)})
	addFocusedCitations(b.inv)
	_, visible := encodeInvestigation(b.inv)
	var schema struct {
		Properties struct {
			Observations struct {
				Items struct {
					Properties struct {
						Citations struct {
							Items struct {
								Enum []string `json:"enum"`
							} `json:"items"`
						} `json:"citations"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"observations"`
		} `json:"properties"`
	}
	raw := investigationSchema(b.inv, visible)
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	ids := schema.Properties.Observations.Items.Properties.Citations.Items.Enum
	if len(ids) == 0 {
		t.Fatal("usable evidence was omitted")
	}
	if _, citation, _ := b.inv.ResolveCitation(ids[0]); citation.LineStart == 0 {
		t.Fatal("schema lost the diagnostic log target priority")
	}
	if strings.Index(string(raw), `"observations":{`) > strings.Index(string(raw), `"hypotheses":{`) || !strings.Contains(string(raw), `"properties":{"text":`) {
		t.Fatal("schema should generate observations first and claims before their citations")
	}
	for _, id := range ids {
		if id == "E3" || id == changed.ID || id == large.ID || !visible[id] {
			t.Fatalf("schema permits unusable or untransmitted source %s", id)
		}
	}
	if schema := investigationSchema(b.inv, map[string]bool{"E3": true, changed.ID: true, "E999": true}); schema != nil {
		t.Fatal("no usable targets should prevent a structured request")
	}
}

type structuredSummaryStub struct {
	plainCalls, structuredCalls int
	schema                      json.RawMessage
	content                     string
}

func (m *structuredSummaryStub) Complete(context.Context, []Message, []ToolDefinition) (Message, error) {
	m.plainCalls++
	return Message{}, nil
}

func (m *structuredSummaryStub) CompleteStructured(_ context.Context, _ []Message, schema json.RawMessage) (Message, error) {
	m.structuredCalls++
	m.schema = schema
	return answer(m.content), nil
}

func TestCrashLoopUsesStructuredRequestAndStillValidatesResponse(t *testing.T) {
	for _, test := range []struct {
		name, content string
		accepted      bool
	}{
		{"valid", `{"observations":[{"text":"The captured pod is restarting.","citations":["E4"]}],"hypotheses":[],"next_checks":[]}`, true},
		{"fabricated_citation", `{"observations":[{"text":"Unsupported claim.","citations":["E999"]}],"hypotheses":[],"next_checks":[]}`, false},
		{"unavailable_source", `{"observations":[{"text":"Unsupported claim.","citations":["E3"]}],"hypotheses":[],"next_checks":[]}`, false},
		{"malformed", "```json\n{}\n```", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := captureFixture()
			m := &structuredSummaryStub{content: test.content}
			result := NewWithModel(b, config.DefaultModelSettings(), false, m).InvestigateCrashLoop(context.Background(), b.resources[0].Ref, "app", nil)
			if m.plainCalls != 0 || m.structuredCalls != 1 || !json.Valid(m.schema) {
				t.Fatal("structured capability lost or extra requests made")
			}
			if accepted := strings.HasPrefix(result.Text, "**Captured observations**"); accepted != test.accepted {
				t.Fatalf("unexpected validation result: %s", result.Text)
			}
			if !test.accepted && (!strings.Contains(result.Text, "discarded") || !strings.Contains(result.Text, "DATABASE_URL")) {
				t.Fatal("rejected summary did not retain captured observations")
			}
		})
	}
}
