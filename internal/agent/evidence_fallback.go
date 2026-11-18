package agent

import (
	"encoding/json"
	"fmt"
	"kubix/internal/core"
	"sort"
	"strings"
)

// uncitedEvidenceSummary never reuses rejected model prose. It reports only
// deterministic comparisons over resources actually retrieved in this answer.
func uncitedEvidenceSummary(resources map[string]core.Resource, order []string, refs map[string]string, evidence []core.ToolResult) string {
	lines := []string{"The model did not cite its retrieved resources. Its answer was discarded.", "", "**Collected observations (not a model diagnosis)**"}
	for _, key := range order {
		r := resources[key]
		lines = append(lines, fmt.Sprintf("- Captured %s: %s. [%s]", r.Ref.DisplayName(), r.Status, refs[key]))
		if len(lines) >= 23 {
			lines = append(lines, "- Additional retrieved resources remain available in the resource table.")
			break
		}
	}
	comparisons := 0
	for _, sk := range order {
		service := resources[sk]
		if service.Ref.Kind != "services" {
			continue
		}
		selector, ok := stringFields(service.Details["selector"])
		if !ok || len(selector) == 0 {
			continue
		}
		for _, pk := range order {
			pod := resources[pk]
			if pod.Ref.Kind != "pods" || pod.Ref.Namespace != service.Ref.Namespace {
				continue
			}
			labels, ok := stringFields(pod.Details["labels"])
			if !ok {
				continue
			}
			keys := make([]string, 0, len(selector))
			for key := range selector {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				value, found := labels[key]
				if !found && pod.Details["labels_truncated"] == true {
					continue
				}
				if found && value == selector[key] {
					continue
				}
				actual := "not present in the captured label map"
				if found {
					actual = fmt.Sprintf("%q", value)
				}
				lines = append(lines, fmt.Sprintf("- Service %s selector %q=%q does not match captured Pod %s label %q (%s). This selector excludes that Pod in the captured state. [%s] [%s]", service.Ref.Name, key, selector[key], pod.Ref.Name, key, actual, refs[sk], refs[pk]))
				comparisons++
				break
			}
			if comparisons >= 10 {
				break
			}
		}
		if comparisons >= 10 {
			break
		}
	}
	gaps := map[string]bool{}
	for _, result := range evidence {
		for _, err := range result.Errors {
			gaps[err] = true
		}
	}
	if len(gaps) > 0 {
		lines = append(lines, "", "**Evidence gaps**")
		errors := make([]string, 0, len(gaps))
		for err := range gaps {
			errors = append(errors, err)
		}
		sort.Strings(errors)
		for _, err := range errors {
			lines = append(lines, "- "+err)
		}
		lines = append(lines, "- A denied or failed read does not establish that resources or endpoints are absent.")
	}
	lines = append(lines, "", "These are bounded, sequential reads, not a complete or atomic cluster snapshot. A selector mismatch does not establish that it is the only routing problem. No changes were executed.")
	return strings.Join(lines, "\n")
}

func stringFields(value any) (map[string]string, bool) {
	if value == nil {
		return nil, false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var result map[string]string
	if json.Unmarshal(data, &result) != nil || result == nil {
		return nil, false
	}
	return result, true
}
