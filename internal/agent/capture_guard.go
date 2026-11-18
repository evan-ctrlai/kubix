package agent

import (
	"encoding/json"
	"regexp"
	"strings"

	"kubix/internal/core"
)

// These match explicit diagnostic annotations, not ordinary prose containing
// "cached", "current" or "now". They are a narrow ambiguity signal, never a
// classifier of whether an application's historical claim is actually true.
var historicalDiagnosticMarker = regexp.MustCompile(`(?i)(?:^|\s)(?:STALE(?:\s*\([^)]*\))?|HISTORICAL\s+(?:DIAGNOSIS|ERROR|FAILURE)|PREVIOUS\s+DIAGNOSIS)\s*:`)
var failureLogMarker = regexp.MustCompile(`(?i)\b(fatal|panic|exception|error|failed|failure|missing|denied|killed)\b`)

// logStream obtains temporal identity from collector metadata, not log text.
// Old synthetic/test captures without a query can use collector-generated titles.
func logStream(o core.Observation) string {
	if o.Source != "kubernetes/pods/log" {
		return ""
	}
	for _, field := range strings.Fields(o.Query) {
		if field == "previous=true" {
			return "previous"
		}
		if field == "previous=false" {
			return "current"
		}
	}
	if o.Query != "" {
		return "unknown"
	}
	title := strings.ToLower(o.Title)
	if strings.HasPrefix(title, "previous logs") || strings.HasPrefix(title, "previous container logs") {
		return "previous"
	}
	if strings.HasPrefix(title, "current logs") || strings.HasPrefix(title, "current container logs") {
		return "current"
	}
	return "unknown"
}

func mixedDiagnosticNarrative(o core.Observation) bool {
	if logStream(o) != "current" || o.Format != "log" || (o.State != "sampled" && o.State != "captured") {
		return false
	}
	historical, otherFailure := false, false
	for _, line := range strings.Split(o.Payload, "\n") {
		annotated := historicalDiagnosticMarker.MatchString(line)
		if annotated && failureLogMarker.MatchString(line) {
			historical = true
		}
		if !annotated && failureLogMarker.MatchString(line) {
			otherFailure = true
		}
	}
	return historical && otherFailure
}

// Conservative fallback gates do not establish a diagnosis. The mixed-log
// gate requires two diagnostic lines in one current-log source: an explicit
// historical diagnosis and a separate failure. It does not compare keywords
// across workloads, streams or unrelated normal cache messages.
func conservativeCaptureReason(inv *core.Investigation) string {
	mixed, missingPrevious, healthy := false, false, false
	for _, o := range inv.Observations {
		if o.Ref.Kind != inv.Target.Kind || o.Ref.Namespace != inv.Target.Namespace || o.Ref.Name != inv.Target.Name || o.Ref.UID != inv.Target.UID {
			continue
		}
		if o.Source == "kubernetes/pods/log" {
			if (o.State == "unavailable" || o.State == "changed") && logStream(o) == "previous" {
				missingPrevious = true
			}
			mixed = mixed || mixedDiagnosticNarrative(o)
		}
		if o.Source != "kubernetes/pods" || o.Format != "json" || o.State != "captured" {
			continue
		}
		var r core.Resource
		if json.Unmarshal([]byte(o.Payload), &r) != nil || r.Ref != inv.Target || r.Status != "Running" {
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
	if mixed {
		return "A current-log sample contains an explicitly historical diagnostic claim alongside a separate failure message. Showing source-labelled evidence only; the application's annotations do not establish which explanation is true."
	}
	if healthy && missingPrevious {
		return "The captured containers are Ready with zero restarts, but previous logs are unavailable. Earlier failure or recovery cannot be established from this capture; showing observations without a model diagnosis."
	}
	return ""
}
