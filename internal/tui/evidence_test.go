package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"kubix/internal/agent"
	"kubix/internal/config"
	"kubix/internal/core"
	"kubix/internal/demo"
)

func testCapture(id, payload string) *core.Investigation {
	ref := core.ResourceRef{Kind: "pods", Namespace: "demo", Name: "checkout-abc", UID: "uid-checkout-abc"}
	i := &core.Investigation{ID: id, Kind: "crashloop", Context: "test-context", Namespace: "demo", Target: ref}
	i.AddObservation(core.Observation{
		Title: "Previous app logs", Source: "pods/log", Ref: ref,
		CapturedAt: time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC),
		State:      "sampled", Query: "container=app previous=true tail=100", Format: "log", Payload: payload,
		Notes: []string{"Last 100 lines; older entries may be omitted."},
	})
	return i
}

func publishCapture(m *Model, capture *core.Investigation, answer string) {
	m.askGeneration++
	m.turns = append(m.turns, turn{question: "Why is checkout restarting?", answer: "Investigating…"})
	m.Update(answerMsg{m.askGeneration, core.Answer{Text: answer, Investigation: capture}})
}

func findAnchor(t *testing.T, m *Model, investigationID, citationID string) citationAnchor {
	t.Helper()
	for _, anchor := range m.citationAnchors {
		if anchor.item.investigation.ID == investigationID && anchor.item.citationID == citationID {
			return anchor
		}
	}
	t.Fatalf("no rendered anchor for %s/%s", investigationID, citationID)
	return citationAnchor{}
}

func clickVisibleAnchor(t *testing.T, m *Model, anchor citationAnchor) {
	t.Helper()
	m.conversation.SetYOffset(max(0, anchor.y-2))
	x := m.layout.conversation.x + anchor.x
	y := m.layout.conversation.y + anchor.y - m.conversation.YOffset
	if !m.layout.conversation.contains(x, y) {
		t.Fatal("anchor not in visible conversation")
	}
	// Check the actual composed view, not just the same internal arithmetic used
	// by hit testing. This catches offsets from padding, headings and wrapping.
	line := strings.Split(m.View(), "\n")[y]
	if got := ansi.Strip(ansi.Cut(line, x, x+anchor.width)); got != "["+anchor.item.citationID+"]" {
		t.Fatalf("hit box does not cover rendered citation: %q at %d,%d", got, x, y)
	}
	m.Update(tea.MouseMsg{X: x + 1, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
}

func TestCapturedCitationClickUsesOriginalTurnAcrossScrollAndResize(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(profile)
	m, b, _ := testModel(t)
	old := testCapture("older", "original failure\nmissing config")
	publishCapture(m, old, strings.Repeat("Extra context for scrolling.\n\n", 15)+"**界 checkout** failed in the captured logs [E1].\n\n"+strings.Repeat("Further analysis.\n\n", 15))
	publishCapture(m, testCapture("newer", "replacement is healthy"), "The next investigation has different logs [E1].")
	beforeReads := len(b.detailCalls) + len(b.logCalls) + len(b.listCalls)
	for _, size := range [][2]int{{140, 44}, {80, 24}, {120, 36}} {
		m.resize(size[0], size[1])
		anchor := findAnchor(t, m, "older", "E1")
		clickVisibleAnchor(t, m, anchor)
		if m.captured == nil || m.captured.item.investigation.ID != "older" || !strings.Contains(m.detailText, "original failure") || strings.Contains(m.detailText, "replacement is healthy") {
			t.Fatalf("old E1 resolved against a later turn: %s", m.detailText)
		}
		if !strings.Contains(ansi.Strip(m.View()), "CAPTURED") {
			t.Fatal("captured view lacks a visible distinction from live reads")
		}
	}
	if reads := len(b.detailCalls) + len(b.logCalls) + len(b.listCalls); reads != beforeReads {
		t.Fatalf("citation selection made %d live calls", reads-beforeReads)
	}
}

type typedCrashLoopInvestigator struct {
	fakeInvestigator
	target    core.ResourceRef
	container string
}

func (a *typedCrashLoopInvestigator) InvestigateCrashLoop(_ context.Context, target core.ResourceRef, container string, onEvent func(string)) core.Answer {
	a.target, a.container = target, container
	onEvent("Capturing previous logs")
	return core.Answer{Text: "Captured evidence [E1].", Investigation: testCapture("typed", "captured")}
}

func TestInvestigateShortcutPassesExplicitContainerToTypedInvestigator(t *testing.T) {
	m, b, _ := testModel(t)
	a := &typedCrashLoopInvestigator{}
	m.investigator = a
	m.container = "sidecar"
	m.containerChosen = true
	press(m, "esc")
	runCommand(t, m, press(m, "i"))
	if a.container != "sidecar" || a.target.Key() != b.resources[0].Ref.Key() || len(a.questions) != 0 {
		t.Fatal("typed investigation lost explicit container/identity or fell back to freeform Ask")
	}
	if len(m.citationAnchors) != 1 || m.turns[0].investigation == nil {
		t.Fatal("typed investigation did not retain captured evidence")
	}
}

func TestInvestigateAutomaticContainerIsNotOverriddenByInspectorDefault(t *testing.T) {
	m, _, _ := testModel(t)
	a := &typedCrashLoopInvestigator{}
	m.investigator = a
	runCommand(t, m, m.inspect("details"))
	if m.container == "" || m.containerChosen {
		t.Fatal("inspector did not assign an implicit container default")
	}
	press(m, "esc")
	runCommand(t, m, press(m, "i"))
	if a.container != "" {
		t.Fatal("an unchosen inspector default overrode automatic crash-container selection")
	}
	runCommand(t, m, press(m, "c"))
	runCommand(t, m, press(m, "i"))
	if a.container != "sidecar" || !m.containerChosen {
		t.Fatal("user's explicit container choice was not passed to collector")
	}
	m.moveCursor(1)
	if m.containerChosen {
		t.Fatal("container choice leaked to another selected resource")
	}
}

func TestSingleContainerCanBeExplicitlySelectedForInvestigation(t *testing.T) {
	m, b, _ := testModel(t)
	b.resources = []core.Resource{fakePod("healthy", "app")}
	m.setResources(b.resources)
	a := &typedCrashLoopInvestigator{}
	m.investigator = a
	runCommand(t, m, m.inspect("details"))
	if m.container != "app" || m.containerChosen {
		t.Fatalf("inspector default should remain implicit: container=%q chosen=%v", m.container, m.containerChosen)
	}
	press(m, "esc")
	runCommand(t, m, press(m, "c"))
	if m.container != "app" || !m.containerChosen {
		t.Fatalf("single container was not made explicitly selectable: container=%q chosen=%v", m.container, m.containerChosen)
	}
	runCommand(t, m, press(m, "i"))
	if a.container != "app" || a.target.Name != "healthy" {
		t.Fatalf("explicit healthy-container choice did not reach collector: target=%v container=%q", a.target, a.container)
	}
}

func TestCrashLoopCapturedDemoSnapshot(t *testing.T) {
	path := os.Getenv("KUBIX_CRASHLOOP_SNAPSHOT_PATH")
	if path != "" {
		profile := lipgloss.ColorProfile()
		lipgloss.SetColorProfile(termenv.TrueColor)
		defer lipgloss.SetColorProfile(profile)
	}
	b := demo.New()
	a := agent.New(b, config.DefaultModelSettings(), true)
	m := New(context.Background(), b, a, true)
	defer m.Close()
	m.resize(140, 44)
	runCommand(t, m, m.refresh())
	for index, resource := range m.resources {
		if resource.Status == "CrashLoopBackOff" {
			m.cursor = index
			m.selectCurrent()
			break
		}
	}
	press(m, "esc")
	runCommand(t, m, press(m, "i"))
	if len(m.turns) == 0 || m.turns[0].investigation == nil {
		t.Fatal("real demo collector/agent returned no captured investigation")
	}
	capture := m.turns[0].investigation
	var exactLine string
	for _, citation := range capture.Citations {
		observation, _, ok := capture.ResolveCitation(citation.ID)
		if ok && citation.LineStart > 0 && strings.Contains(observation.Title, "Previous") {
			exactLine = citation.ID
			break
		}
	}
	if exactLine == "" {
		t.Fatal("real demo investigation did not expose an exact previous-log-line citation")
	}
	clickVisibleAnchor(t, m, findAnchor(t, m, capture.ID, exactLine))
	if m.captured == nil || m.captured.citation.LineStart == 0 || !strings.Contains(m.detailText, "DATABASE_URL") {
		t.Fatalf("real demo citation did not open the captured failure line: %s", m.detailText)
	}
	if path != "" {
		if err := os.WriteFile(path, []byte(m.View()), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

type citationNarrativeBackend struct {
	*demo.Backend
}

const citationShapedLogText = "[E1] [E**2**] &#91;E3&#93; &lbrack;E4&rbrack; &#x5b;E5&#x5d; &amp;#91;E6&amp;#93;"

func (b *citationNarrativeBackend) CollectCrashLoop(ctx context.Context, target core.ResourceRef, container string, emit func(string)) *core.Investigation {
	capture := b.Backend.CollectCrashLoop(ctx, target, container, emit)
	for index := range capture.Observations {
		observation := &capture.Observations[index]
		if observation.Format == "log" && observation.State == "sampled" {
			observation.Payload = "ERROR " + observation.Title + ": user-supplied " + citationShapedLogText
		}
	}
	return capture
}

func TestCapturedLogNarrativeCannotForgeRenderedCitationAnchors(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(profile)
	b := &citationNarrativeBackend{demo.New()}
	a := agent.New(b, config.DefaultModelSettings(), true)
	m := New(context.Background(), b, a, true)
	defer m.Close()
	m.resize(140, 44)
	runCommand(t, m, m.refresh())
	press(m, "esc")
	runCommand(t, m, press(m, "i"))
	if len(m.turns) != 1 || m.turns[0].investigation == nil {
		t.Fatal("real demo workflow produced no capture")
	}
	answer := m.turns[0].answer
	capture := m.turns[0].investigation
	// Complete-observation IDs E1…E6 exist in this capture, making these forged
	// tokens resolvable if source prose accidentally turns into a clickable link.
	if !strings.Contains(answer, "［E1］") || !strings.Contains(answer, "［E**2**］") || !strings.Contains(answer, "［E3］") {
		t.Fatalf("source citation lookalikes were not neutralized before Markdown: %s", answer)
	}
	expected := map[string]int{}
	for _, token := range evidenceToken.FindAllStringSubmatch(answer, -1) {
		_, citation, ok := capture.ResolveCitation(token[1])
		if !ok || (citation.JSONPointer == "" && citation.LineStart == 0) {
			t.Fatalf("source prose introduced a non-issued citation %s", token[1])
		}
		expected[token[1]]++
	}
	if len(expected) == 0 {
		t.Fatal("test did not retain genuine generated references")
	}
	for _, size := range [][2]int{{140, 44}, {80, 24}} {
		m.resize(size[0], size[1])
		actual := map[string]int{}
		var logAnchor *citationAnchor
		for _, anchor := range m.citationAnchors {
			actual[anchor.item.citationID]++
			_, citation, _ := capture.ResolveCitation(anchor.item.citationID)
			if citation.LineStart > 0 {
				copy := anchor
				logAnchor = &copy
			}
		}
		if len(actual) != len(expected) {
			t.Fatalf("rendering introduced citation links: actual %v, issued %v", actual, expected)
		}
		for id, count := range actual {
			if expected[id] != count {
				t.Fatalf("rendering introduced citation links for %s: got %d want %d", id, count, expected[id])
			}
		}
		if logAnchor == nil {
			t.Fatal("genuine exact-log-line citation lost its anchor")
		}
		clickVisibleAnchor(t, m, *logAnchor)
		if m.captured == nil || !strings.Contains(m.captured.observation.Payload, citationShapedLogText) || !strings.Contains(m.detailText, citationShapedLogText) {
			t.Fatal("narrative neutralization altered the immutable source log")
		}
	}
}

func TestCapturedPayloadIsClonedAndProtectedFromLiveResults(t *testing.T) {
	m, b, _ := testModel(t)
	inv := testCapture("immutable", "captured failure")
	publishCapture(m, inv, "Observed failure [E1].")
	inv.Observations[0].Payload = "mutated caller payload"
	inv.Observations[0].Notes[0] = "mutated caller note"
	b.resources[0].Details["message"] = "new live resource"
	liveCommand := m.inspect("details")
	anchor := findAnchor(t, m, "immutable", "E1")
	if !m.openCaptured(anchor.item) {
		t.Fatal("captured evidence failed to open")
	}
	want := m.detailText
	if !strings.Contains(want, "captured failure") || strings.Contains(want, "mutated") {
		t.Fatal("turn's evidence aliases caller-owned data")
	}
	runCommand(t, m, liveCommand)
	if m.detailText != want {
		t.Fatal("late live details replaced captured evidence")
	}
	m.Update(listResultMsg{generation: m.listGeneration, resources: []core.Resource{b.resources[1]}})
	if m.detailText != want || m.captured == nil {
		t.Fatal("background list selection replaced captured evidence")
	}
	m.Update(answerMsg{m.askGeneration, core.Answer{Text: "Later result", Resources: b.resources}})
	if m.detailText != want || m.captured == nil {
		t.Fatal("answer's discovered resources replaced captured evidence")
	}
	runCommand(t, m, m.inspect("details"))
	if m.captured != nil || m.inspectorMode != "details" || m.detailText == want {
		t.Fatal("explicit live details did not leave captured mode")
	}
}

func TestEvidencePickerKeyboardAndPromptTyping(t *testing.T) {
	m, b, _ := testModel(t)
	publishCapture(m, testCapture("older", "old log"), "Old finding [E1].")
	publishCapture(m, testCapture("newer", "new log"), "New finding [E1].")
	press(m, "e")
	press(m, "i")
	if m.prompt.Value() != "ei" || m.evidenceMenu {
		t.Fatal("evidence shortcuts consumed ordinary prompt letters")
	}
	press(m, "esc")
	before := len(b.detailCalls) + len(b.logCalls) + len(b.listCalls)
	press(m, "e")
	if !m.evidenceMenu || len(m.evidenceItems) != 2 {
		t.Fatal("evidence picker did not include both turns")
	}
	press(m, "down")
	press(m, "enter")
	if m.evidenceMenu || m.captured == nil || m.captured.item.investigation.ID != "older" || m.focus != inspectorPane {
		t.Fatal("keyboard picker failed to open old-turn evidence")
	}
	if before != len(b.detailCalls)+len(b.logCalls)+len(b.listCalls) {
		t.Fatal("keyboard evidence inspection called live backend")
	}
}

func TestCapturedFieldAndLogExcerptMetadata(t *testing.T) {
	inv := testCapture("metadata", "first\npermission denied password=hidden\nthird\n")
	inv.Observations[0].Errors = []string{"One log stream unavailable"}
	inv.Limitations = []string{"This is not the full log history"}
	c, err := inv.AddCitation(core.EvidenceCitation{ObservationID: "O1", Label: "Failure line", LineStart: 2, LineEnd: 2})
	if err != nil {
		t.Fatal(err)
	}
	o, c, ok := inv.ResolveCitation(c.ID)
	if !ok {
		t.Fatal("valid captured line did not resolve")
	}
	text := capturedText(inv, o, c)
	for _, want := range []string{"2026-11-13T12:30:00Z", "sampled", "uid-checkout-abc", "pods/log", "previous=true", "▶ 2 │ permission denied", "Evidence gaps:", "Last 100 lines", "not the full log history"} {
		if !strings.Contains(text, want) {
			t.Fatalf("captured display lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "hidden") {
		t.Fatal("captured text exposed a credential")
	}
	inv.AddObservation(core.Observation{Title: "Pod status", Format: "json", Payload: `{"details":{"reason":"CrashLoopBackOff","password":"hidden"}}`})
	field, err := inv.AddCitation(core.EvidenceCitation{ObservationID: "O2", JSONPointer: "/details/reason", Label: "Waiting reason"})
	if err != nil {
		t.Fatal(err)
	}
	o, field, _ = inv.ResolveCitation(field.ID)
	text = capturedText(inv, o, field)
	if !strings.Contains(text, "▶ CITED FIELD /details/reason\nCrashLoopBackOff") || strings.Contains(text, "hidden") {
		t.Fatalf("wrong captured field excerpt: %s", text)
	}
}

func TestUnknownCitationCannotOpenEvidence(t *testing.T) {
	m, _, _ := testModel(t)
	publishCapture(m, testCapture("valid", "evidence"), "Valid [E1], invented [E999].")
	if len(m.citationAnchors) != 1 || m.citationAnchors[0].item.citationID != "E1" {
		t.Fatal("unresolved citation became clickable")
	}
}

func TestLongCapturedAnswerLandsAtItsFindings(t *testing.T) {
	m, _, _ := testModel(t)
	publishCapture(m, testCapture("first", "older"), strings.Repeat("Old findings.\n\n", 30)+"Old source [E1].")
	publishCapture(m, testCapture("latest", "new"), "## PRIMARY FINDING\n\nThe app exits during startup [E1].\n\n"+strings.Repeat("Additional supporting context.\n\n", 40))
	if !strings.Contains(ansi.Strip(m.conversation.View()), "PRIMARY FINDING") || m.conversation.AtBottom() {
		t.Fatal("a long investigation hid its primary findings at completion")
	}
	if !strings.Contains(ansi.Strip(m.footer()), "Evidence") || !strings.Contains(ansi.Strip(m.footer()), "Investigate") {
		t.Fatal("new investigation/evidence actions are not discoverable in footer")
	}
}

func TestInvestigateShortcutPreservesDraftAndChecksPod(t *testing.T) {
	m, _, a := testModel(t)
	m.prompt.SetValue("My unfinished question")
	press(m, "esc")
	runCommand(t, m, press(m, "i"))
	if len(a.questions) != 1 || a.questions[0] != "Investigate the selected pod and its current/previous logs" || a.selections[0].Kind != "pods" {
		t.Fatal("investigate shortcut did not submit controlled selected-pod request")
	}
	if m.prompt.Value() != "My unfinished question" {
		t.Fatal("investigate shortcut discarded prompt draft")
	}
	m.selected = &core.ResourceRef{Kind: "deployments", Name: "checkout", Namespace: "demo"}
	if cmd := press(m, "i"); cmd != nil || !strings.Contains(m.activity, "Select a pod") {
		t.Fatal("non-pod shortcut lacks actionable guidance")
	}
}

func TestEvidencePickerFitsSmallTerminalAndResizes(t *testing.T) {
	m, _, _ := testModel(t)
	inv := testCapture("many", "one")
	for n := 0; n < 30; n++ {
		inv.AddObservation(core.Observation{Title: "Another captured source", Format: "log", Payload: "one"})
	}
	publishCapture(m, inv, "Many observations [E1].")
	press(m, "esc")
	press(m, "e")
	for n := 0; n < 25; n++ {
		press(m, "down")
	}
	for _, size := range [][2]int{{140, 44}, {80, 24}, {65, 22}} {
		m.resize(size[0], size[1])
		m.moveEvidence(0)
		lines := strings.Split(m.View(), "\n")
		if len(lines) != size[1] {
			t.Fatal("evidence picker changed terminal height")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) != size[0] {
				t.Fatalf("evidence picker overflowed width %d", size[0])
			}
		}
	}
}
