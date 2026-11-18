package core

import (
	"strings"
	"testing"
)

func TestCapturedCitationTargets(t *testing.T) {
	i := NewInvestigation("test", "demo", ResourceRef{Kind: "pods", Namespace: "demo", Name: "app", UID: "old"})
	full := i.AddObservation(Observation{Format: "json", Payload: `{"details":{"containers":[{"name":"app"}],"a/b":{"~value":false}}}`})
	field, err := i.AddCitation(EvidenceCitation{ObservationID: full.ObservationID, JSONPointer: "/details/a~1b/~0value"})
	if err != nil {
		t.Fatal(err)
	}
	observation, citation, ok := i.ResolveCitation(field.ID)
	value, found := JSONPointerValue(observation.Payload, citation.JSONPointer)
	if !ok || !found || value != false {
		t.Fatalf("escaped field did not resolve: %#v %#v", citation, value)
	}
	for _, pointer := range []string{"details", "/details/missing", "/details/containers/01", "/details/containers/-1", "/details/containers/1", "/details/a~2b"} {
		if _, err := i.AddCitation(EvidenceCitation{ObservationID: full.ObservationID, JSONPointer: pointer}); err == nil {
			t.Errorf("accepted invalid field target %q", pointer)
		}
	}
	logs := i.AddObservation(Observation{Format: "log", Payload: "one\ntwo\nthree\n"})
	line, err := i.AddCitation(EvidenceCitation{ObservationID: logs.ObservationID, LineStart: 2})
	if err != nil || line.LineEnd != 2 {
		t.Fatalf("single-line citation: %#v %v", line, err)
	}
	for _, target := range []EvidenceCitation{
		{ObservationID: "foreign"},
		{ObservationID: full.ObservationID, LineStart: 1},
		{ObservationID: full.ObservationID, JSONPointer: "/details", LineStart: 1},
		{ObservationID: logs.ObservationID, LineStart: 4},
		{ObservationID: logs.ObservationID, LineStart: 2, LineEnd: 1},
		{ObservationID: logs.ObservationID, JSONPointer: "/details"},
	} {
		if _, err := i.AddCitation(target); err == nil {
			t.Errorf("accepted invalid citation %#v", target)
		}
	}
	if _, _, ok := i.ResolveCitation("E999"); ok {
		t.Fatal("resolved an uncaptured citation")
	}
}

func TestCloneRetainsIndependentEvidence(t *testing.T) {
	i := NewInvestigation("test", "demo", ResourceRef{Name: "app", UID: "old"})
	i.Resources = []Resource{{Details: map[string]any{"containers": []any{map[string]any{"name": "old"}}}}}
	i.Limitations = []string{"original limitation"}
	errors := []string{"original error"}
	citation := i.AddObservation(Observation{Payload: "captured text", Format: "log", Errors: errors, Notes: []string{"original note"}})
	errors[0] = "caller mutation"
	copy := i.Clone()
	i.Resources[0].Details["containers"].([]any)[0].(map[string]any)["name"] = "changed"
	i.Observations[0].Payload = "changed text"
	i.Observations[0].Errors[0] = "changed error"
	i.Observations[0].Notes[0] = "changed note"
	i.Citations[0].Label = "changed label"
	i.Limitations[0] = "changed limitation"
	i.Target.UID = "new"
	observation, _, ok := copy.ResolveCitation(citation.ID)
	if !ok || observation.Payload != "captured text" || observation.Errors[0] != "original error" || observation.Notes[0] != "original note" || copy.Target.UID != "old" || copy.Limitations[0] != "original limitation" {
		t.Fatalf("snapshot aliases original data: %+v", copy)
	}
	if strings.Contains(copy.Resources[0].Details["containers"].([]any)[0].(map[string]any)["name"].(string), "changed") {
		t.Fatal("snapshot resource aliases live details")
	}
}
