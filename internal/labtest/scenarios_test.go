package labtest

import (
	"encoding/json"
	"kubix/internal/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScenarioOptionsFailClosed(t *testing.T) {
	good := ScenarioOptions{AllowLive: true, Scenario: "healthy-history", Prefix: "kubix-check-123456abcdef"}
	if err := validScenarioOptions(good); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ScenarioOptions){func(o *ScenarioOptions) { o.AllowLive = false }, func(o *ScenarioOptions) { o.Prefix = "checkout" }, func(o *ScenarioOptions) { o.Scenario = "unknown" }, func(o *ScenarioOptions) { o.ModelURL = "https://example.com/v1"; o.Model = "qwen" }, func(o *ScenarioOptions) { o.ModelURL = "http://127.0.0.1:11436/v1" }, func(o *ScenarioOptions) { o.ModelURL = "http://user:secret@127.0.0.1/v1"; o.Model = "qwen" }} {
		o := good
		change(&o)
		if validScenarioOptions(o) == nil {
			t.Fatalf("accepted unsafe options: %#v", o)
		}
	}
}
func TestScenarioReportRemainsLocal(t *testing.T) {
	root := t.TempDir()
	r := ScenarioReport{Scenario: "healthy-history", Checks: map[string]bool{"fixture": true}}
	path, err := SaveScenarioReport(root, "kubix-check-123456abcdef", r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, filepath.Join(root, ".local", "lab")) {
		t.Fatal(path)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err = SaveScenarioReport(root, "../escape", r); err == nil {
		t.Fatal("allowed path traversal")
	}
}
func TestServiceScenarioRequiresResolvedCitationsAndFixtureEvidence(t *testing.T) {
	p := "kubix-check-123456abcdef"
	a := core.Answer{Text: p + "-pod: selector validation-wrong does not match validation-real [R1] [R2]. 403 does not establish resources absent.", Resources: []core.Resource{{Ref: core.ResourceRef{Kind: "services", Namespace: labNamespace, Name: p + "-svc", UID: "svc"}, Details: map[string]any{"selector": map[string]string{"app": "validation-wrong"}}}, {Ref: core.ResourceRef{Kind: "pods", Namespace: labNamespace, Name: p + "-pod", UID: "pod"}, Details: map[string]any{"labels": map[string]string{"app": "validation-real"}}}}, Evidence: []core.ToolResult{{Errors: []string{"403 forbidden"}}}}
	checks := scenarioChecks("service-mismatch", p, a, 0)
	for name, ok := range checks {
		if !ok {
			t.Fatalf("valid fixture failed %s", name)
		}
	}
	a.Text += " [R0]"
	if scenarioChecks("service-mismatch", p, a, 0)["resource_citations_resolve"] {
		t.Fatal("accepted zero citation")
	}
	a.Text = strings.ReplaceAll(a.Text, "[R0]", "")
	a.Text += " [R99]"
	if scenarioChecks("service-mismatch", p, a, 0)["resource_citations_resolve"] {
		t.Fatal("accepted nonexistent citation")
	}
	a.Text = strings.ReplaceAll(a.Text, "[R99]", "")
	a.Resources[1].Details = nil
	if scenarioChecks("service-mismatch", p, a, 0)["fixture_selector_evidence"] {
		t.Fatal("accepted absent selector ground truth")
	}
	if scenarioChecks("service-mismatch", p, a, 1)["no_model_diagnosis"] {
		t.Fatal("labelled model call deterministic")
	}
}
func TestHealthyScenarioRequiresCapturedStateAndUnknownHistory(t *testing.T) {
	ref := core.ResourceRef{Kind: "pods", Namespace: labNamespace, Name: "fixture", UID: "uid"}
	inv := core.NewInvestigation(labContext, labNamespace, ref)
	pod := core.Resource{Ref: ref, Status: "Running", Details: map[string]any{"containers": []any{map[string]any{"ready": true, "restart_count": 0}}}}
	raw, _ := json.Marshal(pod)
	c := inv.AddObservation(core.Observation{Source: "kubernetes/pods", Format: "json", State: "captured", Payload: string(raw)})
	inv.AddObservation(core.Observation{Source: "kubernetes/pods/log", Title: "Previous logs", State: "unavailable"})
	inv.AddObservation(core.Observation{Source: "kubernetes/pods/log", Title: "Current logs", State: "sampled", Payload: "healthy replacement running"})
	a := core.Answer{Investigation: inv, Text: "Previous logs unavailable; history cannot be established [" + c.ID + "]"}
	for name, ok := range scenarioChecks("healthy-history", "unused", a, 0) {
		if !ok {
			t.Fatalf("failed %s", name)
		}
	}
	a.Text = "Recovered from earlier error [E999]"
	got := scenarioChecks("healthy-history", "unused", a, 0)
	if got["rendered_citations_resolve"] || got["history_not_invented"] {
		t.Fatal("accepted unsupported recovery/history")
	}
}
