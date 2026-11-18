package agent

import (
	"encoding/json"
	"kubix/internal/core"
	"regexp"
	"strings"
)

var staleLogMarker = regexp.MustCompile(`(?i)\b(stale|cached|historical)\b|previous diagnosis`)
var currentLogMarker = regexp.MustCompile(`(?i)\b(current|now)\b`)

// Conservative fallback gates do not establish a diagnosis. Explicitly mixed
// temporal narratives and a healthy capture with unavailable history are cases
// where a small summary model has overstated the evidence in live validation.
func conservativeCaptureReason(inv *core.Investigation) string {
	stale, current, missingPrevious, healthy := false, false, false, false
	for _, o := range inv.Observations {
		if o.Source == "kubernetes/pods/log" {
			if o.State == "unavailable" && strings.Contains(strings.ToLower(o.Title), "previous") {
				missingPrevious = true
			}
			if (o.State == "sampled" || o.State == "captured") && o.Format == "log" {
				stale = stale || staleLogMarker.MatchString(o.Payload)
				current = current || currentLogMarker.MatchString(o.Payload)
			}
		}
		if o.Source != "kubernetes/pods" || o.Format != "json" || o.State != "captured" {
			continue
		}
		var r core.Resource
		if json.Unmarshal([]byte(o.Payload), &r) != nil || r.Ref.UID != inv.Target.UID || r.Ref.Name != inv.Target.Name || r.Ref.Namespace != inv.Target.Namespace || r.Status != "Running" {
			continue
		}
		cs, ok := r.Details["containers"].([]any)
		if !ok || len(cs) == 0 {
			continue
		}
		all := true
		for _, raw := range cs {
			c, ok := raw.(map[string]any)
			if !ok {
				all = false
				break
			}
			ready, rok := c["ready"].(bool)
			count, cok := c["restart_count"].(float64)
			if !rok || !ready || !cok || count != 0 {
				all = false
			}
		}
		healthy = healthy || all
	}
	if stale && current {
		return "Captured logs contain explicit historical and current markers. Showing source-labelled evidence only; these log claims do not establish which explanation is true."
	}
	if healthy && missingPrevious {
		return "The captured containers are Ready with zero restarts, but previous logs are unavailable. Earlier failure or recovery cannot be established from this capture; showing observations without a model diagnosis."
	}
	return ""
}
