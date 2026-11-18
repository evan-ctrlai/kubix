package agent

import (
	"encoding/json"

	"kubix/internal/core"
)

// The server constrains output shape; the renderer independently verifies every
// reference against the immutable capture and the actual transmitted budget.
func investigationSchema(inv *core.Investigation, visible map[string]bool) json.RawMessage {
	ids := []string{}
	for _, citation := range orderedCaptureCitations(inv) {
		if o, _, ok := inv.ResolveCitation(citation.ID); visible[citation.ID] && ok && (o.State == "captured" || o.State == "sampled") {
			ids = append(ids, citation.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// Preserve the intended generation order for schema-aware local servers:
	// observations before hypotheses, and the claim text before its citations.
	claim := map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"text", "citations"},
		"properties": struct {
			Text      map[string]any `json:"text"`
			Citations map[string]any `json:"citations"`
		}{
			Text: map[string]any{"type": "string", "minLength": 1, "maxLength": 2000},
			Citations: map[string]any{
				"type": "array", "minItems": 1, "maxItems": 6,
				"items": map[string]any{"type": "string", "enum": ids},
			},
		},
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"observations", "hypotheses", "next_checks"},
		"properties": struct {
			Observations map[string]any `json:"observations"`
			Hypotheses   map[string]any `json:"hypotheses"`
			NextChecks   map[string]any `json:"next_checks"`
		}{
			Observations: map[string]any{"type": "array", "minItems": 1, "maxItems": 6, "items": claim},
			Hypotheses:   map[string]any{"type": "array", "maxItems": 6, "items": claim},
			NextChecks: map[string]any{
				"type": "array", "maxItems": 6,
				"items": map[string]any{"type": "string", "minLength": 1, "maxLength": 1000},
			},
		},
	}
	encoded, _ := json.Marshal(schema)
	return encoded
}
