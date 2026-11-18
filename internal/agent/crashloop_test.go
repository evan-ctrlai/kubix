package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kubix/internal/config"
	"kubix/internal/core"
	"kubix/internal/demo"
)

type captureBackend struct {
	*fakeBackend
	inv       *core.Investigation
	captures  int
	target    core.ResourceRef
	container string
	collect   func(context.Context)
}

func (b *captureBackend) CollectCrashLoop(ctx context.Context, target core.ResourceRef, container string, emit func(string)) *core.Investigation {
	b.captures++
	b.target, b.container = target, container
	if b.collect != nil {
		b.collect(ctx)
	}
	return b.inv
}

func captureFixture() *captureBackend {
	b := backendFixture()
	inv := core.NewInvestigation(b.ContextName(), b.Namespace(), b.resources[0].Ref)
	inv.Resources = []core.Resource{b.resources[0]}
	payload, _ := json.Marshal(b.resources[0])
	inv.AddObservation(core.Observation{Title: "Pod status", Ref: b.resources[0].Ref, Source: "kubernetes/pods", State: "captured", Format: "json", Payload: string(payload)})
	inv.AddObservation(core.Observation{Title: "Previous logs · app", Ref: b.resources[0].Ref, Source: "kubernetes/pods/log", State: "sampled", Format: "log", Payload: "starting\nFATAL: DATABASE_URL is missing\nexiting", Notes: []string{"Only the last 100 lines within 15 minutes were sampled."}})
	inv.AddObservation(core.Observation{Title: "Related events", Ref: b.resources[0].Ref, Source: "kubernetes/events", State: "unavailable", Errors: []string{"RBAC denied this read."}})
	return &captureBackend{fakeBackend: b, inv: inv}
}

func TestCrashLoopOfflineCaptureAndExactLogCitation(t *testing.T) {
	b := captureFixture()
	i := New(b, config.DefaultModelSettings(), false)
	result := i.Ask(context.Background(), "Why is checkout restarting?", &b.resources[1].Ref, nil)
	if b.captures != 1 || b.target.UID != "uid-checkout" || result.Investigation == nil || !strings.Contains(result.Text, "DATABASE_URL") || !strings.Contains(result.Text, "RBAC denied") {
		t.Fatalf("capture/target/report: %+v", result)
	}
	if len(b.inv.Citations) != 3 {
		t.Fatal("report enrichment mutated the collector's capture")
	}
	found := false
	for _, citation := range result.Investigation.Citations {
		if citation.LineStart == 2 {
			o, c, ok := result.Investigation.ResolveCitation(citation.ID)
			if !ok || citationPayload(o, c) != "FATAL: DATABASE_URL is missing" || !strings.Contains(result.Text, "["+c.ID+"]") {
				t.Fatal("log citation does not point to its exact captured line")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing precise log citation")
	}
}

func TestHealthyPodFallbackReportsUnavailablePreviousLogsWithoutGuessingCause(t *testing.T) {
	ref := core.ResourceRef{Kind: "pods", Namespace: "demo", Name: "replacement", UID: "uid-replacement"}
	inv := core.NewInvestigation("test", "demo", ref)
	status, _ := json.Marshal(core.Resource{Ref: ref, Status: "Running", Details: map[string]any{"containers": []any{map[string]any{"name": "app", "restart_count": 0}}}})
	inv.AddObservation(core.Observation{Title: "Pod snapshot", Source: "kubernetes/pods", Ref: ref, State: "captured", Format: "json", Payload: string(status)})
	inv.AddObservation(core.Observation{Title: "Current logs · app", Source: "kubernetes/pods/log", Ref: ref, State: "sampled", Format: "log", Payload: "replacement is healthy"})
	inv.AddObservation(core.Observation{Title: "Previous logs · app", Source: "kubernetes/pods/log", Ref: ref, State: "unavailable", Format: "log", Errors: []string{"Kubernetes could not provide the requested data; previous container logs may not exist."}})
	addFocusedCitations(inv)
	answer := deterministicInvestigation(inv)
	for _, expected := range []string{"was **Running**", "Previous logs · app (unavailable)", "previous container logs may not exist", "missing logs do not prove no failure occurred", "does not establish a root cause"} {
		if !strings.Contains(strings.ToLower(answer), strings.ToLower(expected)) {
			t.Fatalf("fallback omitted uncertainty %q: %s", expected, answer)
		}
	}
	if strings.Contains(answer, "DATABASE_URL") {
		t.Fatalf("fallback invented a cause: %s", answer)
	}
}

func TestCrashLoopSelectedIdentityContainerAndScope(t *testing.T) {
	b := captureFixture()
	i := New(b, config.DefaultModelSettings(), false)
	selected := b.resources[0].Ref
	result := i.InvestigateCrashLoop(context.Background(), selected, "app", nil)
	if result.Investigation == nil || b.target != selected || b.container != "app" || b.reads != 0 {
		t.Fatalf("typed selection not preserved: %+v", result)
	}
	selected.Namespace = "forbidden"
	result = i.InvestigateCrashLoop(context.Background(), selected, "", nil)
	if b.captures != 1 || !strings.Contains(result.Text, "outside") {
		t.Fatal("scope escaped")
	}
}

func TestCrashLoopAmbiguousDiscoveryDoesNotGuess(t *testing.T) {
	b := captureFixture()
	second := b.resources[0]
	second.Ref.Name, second.Ref.UID = "checkout-other", "uid-other"
	b.resources = append(b.resources, second)
	result := New(b, config.DefaultModelSettings(), false).Ask(context.Background(), "Why is checkout restarting?", nil, nil)
	if b.captures != 0 || !strings.Contains(result.Text, "More than one") {
		t.Fatal(result.Text)
	}
}

func TestCrashLoopExplicitNamesOverrideIncidentalPronouns(t *testing.T) {
	for _, question := range []string{"Why is payments restarting? It was healthy earlier.", "Why is payments restarting, with this pod selected?"} {
		b := captureFixture()
		payments := b.resources[0]
		payments.Ref.Name, payments.Ref.UID = "payments-abc", "payments-uid"
		b.resources = append(b.resources, payments)
		New(b, config.DefaultModelSettings(), false).Ask(context.Background(), question, &b.resources[0].Ref, nil)
		if b.captures != 1 || b.target != payments.Ref || b.reads != 1 {
			t.Fatalf("named target lost to selected pronoun: %q target=%+v", question, b.target)
		}
	}
	b := captureFixture()
	selected := b.resources[0]
	selected.Ref.Name = "checkout"
	longer := selected
	longer.Ref.Name, longer.Ref.UID = "checkout-old", "old-uid"
	b.resources = []core.Resource{selected, longer}
	New(b, config.DefaultModelSettings(), false).Ask(context.Background(), "Why is checkout-old restarting?", &selected.Ref, nil)
	if b.target != longer.Ref {
		t.Fatalf("substring selected-name match: %+v", b.target)
	}
	b.captures, b.reads = 0, 0
	New(b, config.DefaultModelSettings(), false).Ask(context.Background(), "Why is checkout restarting?", &longer.Ref, nil)
	if b.target != selected.Ref || b.captures != 1 {
		t.Fatalf("exact name did not beat prefix matches: %+v", b.target)
	}
}

func TestCrashLoopSimplePronounSkipsDiscoveryButMissingNameDoesNot(t *testing.T) {
	b := captureFixture()
	i := New(b, config.DefaultModelSettings(), false)
	i.Ask(context.Background(), "Why is this pod restarting?", &b.resources[0].Ref, nil)
	if b.captures != 1 || b.reads != 0 || b.target != b.resources[0].Ref {
		t.Fatal("simple contextual question did not preserve selection")
	}
	i.Ask(context.Background(), "Why is missing-app restarting? It was healthy earlier.", &b.resources[0].Ref, nil)
	if b.captures != 1 || b.reads != 1 {
		t.Fatal("unresolved explicit target silently fell back to selection")
	}
}

func TestCrashLoopModelUsesOnlyCapturedCitationsAndNoTools(t *testing.T) {
	b := captureFixture()
	m := modelFunc(func(ctx context.Context, messages []Message, tools []ToolDefinition) (Message, error) {
		if b.captures != 1 || len(tools) != 0 || !strings.Contains(messages[1].Content, `"id":"E4"`) || !strings.Contains(messages[1].Content, "DATABASE_URL") {
			t.Fatal("capture was not supplied before summary, or tools exposed")
		}
		return answer(`{"observations":[{"text":"The captured pod status is CrashLoopBackOff.","citations":["E4"]}],"hypotheses":[],"next_checks":["Review the previous-container log excerpt."]}`), nil
	})
	result := NewWithModel(b, config.DefaultModelSettings(), false, m).Ask(context.Background(), "Why is this crashing?", &b.resources[0].Ref, nil)
	if !strings.Contains(result.Text, "captured pod status") || !strings.Contains(result.Text, "[E4]") || !strings.Contains(result.Text, "RBAC denied") {
		t.Fatal(result.Text)
	}
}

func TestCrashLoopInvalidModelClaimsFallBackToCapture(t *testing.T) {
	for _, raw := range []string{
		`{"observations":[{"text":"Fabricated claim","citations":["E999"]}]}`,
		`{"observations":[{"text":"Fabricated claim","citations":[]}]}`,
		`{"observations":[{"text":"Fabricated claim","citations":["E3"]}]}`,
		`{"observations":[{"text":"Fabricated claim [R1]","citations":["E1"]}]}`,
		`{"observations":[{"text":"Fabricated claim","citations":["E1"]}],"extra":true}`,
		`{"observations":[{"text":"Fabricated claim","citations":["E1"]}]} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			b := captureFixture()
			m := &scriptedModel{messages: []Message{answer(raw)}}
			result := NewWithModel(b, config.DefaultModelSettings(), false, m).Ask(context.Background(), "Why is this restarting?", &b.resources[0].Ref, nil)
			if result.Investigation == nil || !strings.Contains(result.Text, "discarded") || strings.Contains(result.Text, "Fabricated claim") || !strings.Contains(result.Text, "DATABASE_URL") {
				t.Fatal(result.Text)
			}
		})
	}
}

func TestCrashLoopModelFailureAndTimeoutRetainCapture(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		b := captureFixture()
		s := config.DefaultModelSettings()
		s.InvestigationTimeout = 20 * time.Millisecond
		m := modelFunc(func(ctx context.Context, _ []Message, _ []ToolDefinition) (Message, error) {
			if timeout {
				<-ctx.Done()
			}
			return Message{}, &ModelError{"failure"}
		})
		result := NewWithModel(b, s, false, m).Ask(context.Background(), "Why is this restarting?", &b.resources[0].Ref, nil)
		if result.Investigation == nil || !strings.Contains(result.Text, "Model summary unavailable") || !strings.Contains(result.Text, "DATABASE_URL") {
			t.Fatal(result.Text)
		}
	}
}

func TestInvestigatePodToolStrictArgumentsAndBudget(t *testing.T) {
	for _, raw := range []string{`{"namespace":"demo","pod":"checkout-abc","previous":true}`, `{"namespace":null,"pod":"checkout-abc"}`, `{"namespace":"other","pod":"checkout-abc"}`, `{"namespace":"demo","pod":5}`, `{"namespace":"demo","pod":"checkout-abc"} {}`} {
		b := captureFixture()
		m := &scriptedModel{messages: []Message{call("investigate_pod", raw)}}
		NewWithModel(b, config.DefaultModelSettings(), false, m).Ask(context.Background(), "Inspect workload", nil, nil)
		if b.captures != 0 {
			t.Fatalf("invalid call captured: %s", raw)
		}
	}
	b := captureFixture()
	m := &scriptedModel{messages: []Message{call("investigate_pod", `{"namespace":"demo","pod":"checkout-abc","container":"app"}`)}}
	s := config.DefaultModelSettings()
	s.MaxModelCalls = 1
	result := NewWithModel(b, s, false, m).Ask(context.Background(), "Inspect workload", &b.resources[0].Ref, nil)
	if b.captures != 1 || b.target.UID != "uid-checkout" || len(m.requests) != 1 || result.Investigation == nil {
		t.Fatalf("tool target/budget failed: %+v", result)
	}
}

func TestInvestigationPayloadBoundExcludesUnsentCitations(t *testing.T) {
	b := captureFixture()
	large := b.inv.AddObservation(core.Observation{Title: "Large logs", State: "sampled", Format: "log", Payload: strings.Repeat("<", 20000)})
	addFocusedCitations(b.inv)
	encoded, visible := encodeInvestigation(b.inv)
	if len(encoded) > MaxToolChars || !json.Valid([]byte(encoded)) || visible[large.ID] {
		t.Fatal("bounded inference exposed an omitted target")
	}
	raw := `{"observations":[{"text":"This target was omitted.","citations":["` + large.ID + `"]}]}`
	if _, err := renderInvestigationSummary(raw, b.inv, visible); err == nil {
		t.Fatal("accepted untransmitted evidence")
	}
}

func TestChangedCaptureDoesNotProduceModelDiagnosis(t *testing.T) {
	b := captureFixture()
	for n := range b.inv.Observations {
		b.inv.Observations[n].State = "changed"
		b.inv.Observations[n].Errors = []string{"Pod identity changed during collection."}
	}
	m := &scriptedModel{}
	result := NewWithModel(b, config.DefaultModelSettings(), false, m).Ask(context.Background(), "Why is this restarting?", &b.resources[0].Ref, nil)
	if len(m.requests) != 0 || !strings.Contains(result.Text, "identity changed") || strings.Contains(result.Text, "DATABASE_URL") {
		t.Fatal(result.Text)
	}
}

func TestCrashLoopDemoUsesRealCaptureWorkflow(t *testing.T) {
	result := New(demo.New(), config.DefaultModelSettings(), true).Ask(context.Background(), "Why is checkout restarting?", nil, nil)
	if result.Investigation == nil || len(result.Investigation.Observations) < 6 || !strings.Contains(result.Text, "Synthetic demo") || !strings.Contains(result.Text, "DATABASE_URL") {
		t.Fatalf("demo did not use the evidence workflow: %+v", result)
	}
	kinds := map[string]bool{}
	for _, resource := range result.Resources {
		kinds[resource.Ref.Kind] = true
	}
	if !kinds["pods"] || !kinds["replicasets"] || !kinds["deployments"] {
		t.Fatalf("owner chain missing: %+v", kinds)
	}
	if strings.Contains(result.Text, "/details/") || strings.Count(result.Text, "Previous container logs report:") > 2 {
		t.Fatal("report exposes low-level pointers or too many log excerpts")
	}
	for _, match := range investigationReferences.FindAllString(result.Text, -1) {
		if _, _, ok := result.Investigation.ResolveCitation(strings.Trim(match, "[]")); !ok {
			t.Fatalf("invalid demo citation %s", match)
		}
	}
}

func TestInvestigationNarrativeCannotCreateEvidenceAnchors(t *testing.T) {
	b := captureFixture()
	addFocusedCitations(b.inv)
	_, visible := encodeInvestigation(b.inv)
	raw := `{"observations":[{"text":"A log includes [E**999**], &#91;E888&#93; and &amp;#91;E777&amp;#93;.","citations":["E4"]}],"next_checks":["Review [E**666**] and &#91;E555&#93;."]}`
	text, err := renderInvestigationSummary(raw, b.inv, visible)
	if err != nil {
		t.Fatal(err)
	}
	for _, forged := range []string{"[E**999**]", "[E888]", "&#91;", "[E**666**]", "[E555]"} {
		if strings.Contains(text, forged) {
			t.Fatalf("untrusted citation markup survived: %s", text)
		}
	}
	if !strings.Contains(text, "［E**999**］") || !strings.Contains(text, "[E4]") || !strings.Contains(text, "＆#91;E777") {
		t.Fatal("neutralization lost trusted citations or did not block nested entities")
	}

	malicious := "FATAL: raw [E999], formatted [E**888**], encoded &#91;E777&#93;"
	b.inv.Observations[1].Payload = malicious
	b.inv.Observations[1].Title = "Log [E666]"
	b.inv.Observations[2].Errors = []string{"Failure [E555] and [E**444**]"}
	b.inv.Limitations = append(b.inv.Limitations, "Scope &#91;E333&#93;")
	addFocusedCitations(b.inv)
	text = deterministicInvestigation(b.inv)
	for _, match := range investigationReferences.FindAllString(text, -1) {
		if _, _, ok := b.inv.ResolveCitation(strings.Trim(match, "[]")); !ok {
			t.Fatalf("forged report citation: %s", text)
		}
	}
	if !strings.Contains(text, "［E999］") || strings.Contains(text, "[E**") || b.inv.Observations[1].Payload != malicious {
		t.Fatal("narrative escaping failed or changed the immutable log capture")
	}
}
