package tui

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"sigs.k8s.io/yaml"

	"kubix/internal/agent"
	"kubix/internal/config"
	"kubix/internal/core"
	"kubix/internal/demo"
)

type logCall struct {
	pod, container string
	previous       bool
}

type fakeBackend struct {
	resources       []core.Resource
	listCalls       []string
	detailCalls     []string
	logCalls        []logCall
	limited         bool
	replacementLogs bool
	panicRead       bool
}

func fakePod(name string, containers ...string) core.Resource {
	return core.Resource{
		Ref:    core.ResourceRef{Kind: "pods", Namespace: "demo", Name: name, UID: "uid-" + name},
		Status: "CrashLoopBackOff", Summary: "Repeated restarts",
		Details: map[string]any{"name": name, "containers": containers},
	}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{resources: []core.Resource{fakePod("checkout-abc", "app", "sidecar"), fakePod("payments-xyz", "payments")}}
}
func (*fakeBackend) ContextName() string { return "test-context" }
func (*fakeBackend) Namespace() string   { return "demo" }
func (b *fakeBackend) ListResources(_ context.Context, kind, _, _ string, _ int) core.ToolResult {
	if b.panicRead {
		panic("sensitive-unrecognized-credential")
	}
	b.listCalls = append(b.listCalls, kind)
	return core.ToolResult{Tool: "list_resources", Resources: b.resources, Data: map[string]any{"limited": b.limited}}
}
func (b *fakeBackend) GetResource(_ context.Context, _, _, name string) core.ToolResult {
	if b.panicRead {
		panic("sensitive-unrecognized-credential")
	}
	b.detailCalls = append(b.detailCalls, name)
	for _, r := range b.resources {
		if r.Ref.Name == name {
			return core.ToolResult{Tool: "get_resource", Resources: []core.Resource{r}}
		}
	}
	return core.ErrorResult("get_resource", "Resource no longer exists")
}
func (b *fakeBackend) PodLogs(_ context.Context, _, pod, container string, previous bool) core.ToolResult {
	b.logCalls = append(b.logCalls, logCall{pod, container, previous})
	ref := b.resources[0].Ref
	if b.replacementLogs {
		ref.UID = "replacement-during-logs"
	}
	return core.ToolResult{Tool: "get_pod_logs", Resources: []core.Resource{{Ref: ref}}, Data: map[string]any{"logs": container + " [red]literal log[/red] password=hunter2\x1b[31m", "bounded_sample": true}}
}

type fakeInvestigator struct {
	backend          *fakeBackend
	questions        []string
	selections       []*core.ResourceRef
	empty            bool
	started, release chan struct{}
}

func (*fakeInvestigator) ModeLabel() string { return "DEMO · test model" }
func (a *fakeInvestigator) Ask(ctx context.Context, question string, selected *core.ResourceRef, onEvent func(string)) core.Answer {
	a.questions = append(a.questions, question)
	a.selections = append(a.selections, selected)
	onEvent("Reading pod status")
	if a.started != nil {
		close(a.started)
		select {
		case <-a.release:
		case <-ctx.Done():
		}
	}
	if a.empty {
		return core.Answer{Text: "Configure a model endpoint to investigate. Browsing remains available."}
	}
	return core.Answer{Text: "Checkout is restarting. Inspect previous logs.", Resources: []core.Resource{a.backend.resources[0]}}
}

func testModel(t *testing.T) (*Model, *fakeBackend, *fakeInvestigator) {
	t.Helper()
	b := newFakeBackend()
	a := &fakeInvestigator{backend: b}
	m := New(context.Background(), b, a, true)
	t.Cleanup(m.Close)
	runCommand(t, m, m.refresh())
	return m, b, a
}

// Commands execute synchronously in these tests; an actual Bubble Tea program
// executes each command independently and serializes the resulting messages.
func runCommand(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, next := range batch {
			runCommand(t, m, next)
		}
		return
	}
	_, next := m.Update(msg)
	if _, ok := msg.(progressMsg); ok {
		runCommand(t, m, next)
	}
}

func press(m *Model, key string) tea.Cmd {
	types := map[string]tea.KeyType{"enter": tea.KeyEnter, "esc": tea.KeyEsc, "tab": tea.KeyTab, "up": tea.KeyUp, "down": tea.KeyDown, "ctrl+r": tea.KeyCtrlR, "ctrl+q": tea.KeyCtrlQ, "pgdown": tea.KeyPgDown}
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	if kind, ok := types[key]; ok {
		msg = tea.KeyMsg{Type: kind}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func TestQuestionDiscoveryDetailsAndContainerLogs(t *testing.T) {
	m, b, a := testModel(t)
	if len(m.resources) != 2 {
		t.Fatal("initial pods missing")
	}
	m.prompt.SetValue("Find unhealthy checkout pods")
	runCommand(t, m, press(m, "enter"))
	if m.lastAnswer == nil || len(m.resources) != 1 || m.kind != "discovered" {
		t.Fatal("answer did not populate discoveries")
	}
	press(m, "esc")
	runCommand(t, m, press(m, "enter"))
	if !strings.Contains(m.detailText, "name: checkout-abc") || len(m.containers) != 2 {
		t.Fatal("details or container choices missing")
	}
	runCommand(t, m, press(m, "l"))
	if got := b.logCalls[len(b.logCalls)-1]; got != (logCall{"checkout-abc", "app", false}) {
		t.Fatalf("unexpected log request: %+v", got)
	}
	if !strings.Contains(m.detailText, "[red]literal log[/red]") || strings.Contains(m.detailText, "hunter2") || strings.Contains(m.detailText, "\x1b") {
		t.Fatal("logs were not literal and sanitized")
	}
	runCommand(t, m, press(m, "c"))
	runCommand(t, m, press(m, "p"))
	if got := b.logCalls[len(b.logCalls)-1]; got != (logCall{"checkout-abc", "sidecar", true}) {
		t.Fatalf("container/previous mode not applied: %+v", got)
	}
	press(m, "/")
	m.prompt.SetValue("Why is this restarting?")
	runCommand(t, m, press(m, "enter"))
	if len(a.selections) != 2 || !sameRef(a.selections[1], &b.resources[0].Ref) {
		t.Fatal("follow-up lost selected context")
	}
}

func TestDetailsYAMLPreservesTypesAndRedaction(t *testing.T) {
	m, b, _ := testModel(t)
	b.resources[0].Details = map[string]any{
		"name": "checkout-abc", "password": "hidden-credential",
		"enabled": true, "label": "true",
		"containers": []any{map[string]any{"name": "app", "port": 8080}},
		"message":    "first line\nsecond line\x1b[31m",
	}
	runCommand(t, m, m.inspect("details"))
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(m.detailText), &parsed); err != nil {
		t.Fatalf("Details is not valid YAML: %v\n%s", err, m.detailText)
	}
	if parsed["password"] != "[REDACTED]" || parsed["enabled"] != true || parsed["label"] != "true" {
		t.Fatalf("YAML scalar types or redacted string changed: %#v", parsed)
	}
	if parsed["message"] != "first line\nsecond line" || strings.Contains(m.detailText, "hidden-credential") || strings.Contains(m.detailText, "\x1b") {
		t.Fatal("multiline value or safe display changed")
	}
	containers, ok := parsed["containers"].([]any)
	if !ok || len(containers) != 1 || containers[0].(map[string]any)["port"] != float64(8080) {
		t.Fatalf("nested YAML list changed: %#v", parsed["containers"])
	}
	withGap := resultText(core.ToolResult{Data: b.resources[0].Details, Errors: []string{"request failed: token=hidden-token\ntry again"}}, "details")
	if err := yaml.Unmarshal([]byte(withGap), &parsed); err != nil || !strings.Contains(withGap, "# Evidence gaps:") || strings.Contains(withGap, "hidden-token") {
		t.Fatalf("diagnostic comments broke YAML or redaction: %v\n%s", err, withGap)
	}
}

func TestPromptLettersRemainTextAndPaneNavigation(t *testing.T) {
	m, b, _ := testModel(t)
	for _, key := range []string{"l", "p", "d", "?", "/", "k", "c"} {
		press(m, key)
	}
	if m.prompt.Value() != "lpd?/kc" || len(b.logCalls) != 0 || m.help {
		t.Fatal("shortcuts consumed question characters")
	}
	press(m, "tab")
	if m.focus != resourcesPane {
		t.Fatal("Tab should focus resources")
	}
	press(m, "tab")
	if m.focus != inspectorPane {
		t.Fatal("Tab should focus inspector")
	}
	press(m, "tab")
	if m.focus != conversationPane {
		t.Fatal("Tab should focus conversation")
	}
	press(m, "?")
	if !m.help {
		t.Fatal("help did not open")
	}
	press(m, "esc")
	if m.help {
		t.Fatal("help did not close")
	}
}

func TestEmptyAnswerPreservesKindAndRefresh(t *testing.T) {
	m, b, a := testModel(t)
	a.empty = true
	press(m, "esc")
	runCommand(t, m, press(m, "k"))
	runCommand(t, m, press(m, "k"))
	if m.kind != "services" {
		t.Fatal("resource kind did not cycle")
	}
	press(m, "/")
	m.prompt.SetValue("Why is checkout failing?")
	runCommand(t, m, press(m, "enter"))
	if len(m.resources) != 2 || m.kind != "services" {
		t.Fatal("empty answer erased browser")
	}
	runCommand(t, m, press(m, "ctrl+r"))
	if b.listCalls[len(b.listCalls)-1] != "services" {
		t.Fatal("refresh lost chosen kind")
	}
}

func TestStaleResponsesAndUIDReplacementAreRejected(t *testing.T) {
	m, b, _ := testModel(t)
	old := *m.selected
	cmd := m.inspect("details")
	oldGeneration := m.detailGeneration
	m.moveCursor(1)
	runCommand(t, m, cmd)
	if strings.Contains(m.detailText, "checkout-abc") || m.selected.Name != "payments-xyz" {
		t.Fatal("stale detail overwrote new selection")
	}
	m.Update(detailResultMsg{generation: oldGeneration, ref: old, text: "late old response"})
	if strings.Contains(m.detailText, "late old response") {
		t.Fatal("generation guard failed")
	}
	m.moveCursor(-1)
	b.resources[0].Ref.UID = "replacement-uid"
	runCommand(t, m, m.inspect("logs"))
	if !strings.Contains(m.detailText, "replaced") || len(b.logCalls) != 0 {
		t.Fatal("stale UID allowed log request")
	}
	runCommand(t, m, m.refresh())
	b.replacementLogs = true
	runCommand(t, m, m.inspect("logs"))
	if !strings.Contains(m.detailText, "replaced while loading") {
		t.Fatal("replacement during logs was missed")
	}
	before := m.resources[0].Ref
	m.Update(listResultMsg{generation: m.listGeneration - 1, resources: []core.Resource{fakePod("stale", "app")}})
	if m.resources[0].Ref != before {
		t.Fatal("stale list overwrote resource view")
	}
}

func TestBoundedNoticeErrorsAndContextCancellation(t *testing.T) {
	m, b, _ := testModel(t)
	b.limited = true
	runCommand(t, m, m.refresh())
	if !strings.Contains(m.listNotice, "bounded sample") || !strings.Contains(ansi.Strip(m.View()), "bounded sample") {
		t.Fatal("bounded list notice is hidden")
	}
	b.panicRead = true
	runCommand(t, m, m.inspect("details"))
	if strings.Contains(m.detailText, "sensitive-unrecognized-credential") || !strings.Contains(m.detailText, "read permissions") {
		t.Fatal("unexpected error leaked secret or lacked guidance")
	}
	runCommand(t, m, m.refresh())
	if len(m.resources) != 2 || strings.Contains(m.listNotice, "sensitive-unrecognized-credential") {
		t.Fatal("refresh error erased browser or leaked secret")
	}
	press(m, "ctrl+q")
	if m.ctx.Err() != context.Canceled || !m.quitting {
		t.Fatal("quit did not cancel background operations")
	}
}

func TestOnlyOneInvestigationRunsAndProgressIsMessageBased(t *testing.T) {
	m, _, a := testModel(t)
	a.started = make(chan struct{})
	a.release = make(chan struct{})
	m.prompt.SetValue("first")
	batch := m.ask()().(tea.BatchMsg)
	answer := make(chan tea.Msg, 1)
	go func() { answer <- batch[0]() }()
	select {
	case <-a.started:
	case <-time.After(time.Second):
		t.Fatal("investigation did not start")
	}
	m.prompt.SetValue("second")
	if cmd := m.ask(); cmd != nil || m.prompt.Value() != "second" {
		t.Fatal("concurrent ask was launched or pending text lost")
	}
	progress := batch[1]()
	if _, ok := progress.(progressMsg); !ok {
		t.Fatalf("expected progress message, got %T", progress)
	}
	m.Update(progress)
	if m.activity != "Reading pod status" {
		t.Fatal("progress did not update through a message")
	}
	close(a.release)
	select {
	case msg := <-answer:
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("investigation did not finish")
	}
	if len(a.questions) != 1 || m.asking {
		t.Fatal("investigator concurrency guard failed")
	}
}

func TestMouseSelectionControlsAndKindSwitch(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 36}, {140, 44}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, b, _ := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			clickAt(t, m, m.layout.resourceRows.x+2, m.layout.resourceRows.y)
			if !strings.Contains(m.detailText, "checkout-abc") || m.selected.Name != "checkout-abc" {
				t.Fatal("visible resource row did not inspect its resource")
			}
			for i, label := range []string{"Details", "Logs", "Previous"} {
				button := m.layout.buttons[i]
				if !strings.Contains(screenRect(t, m.View(), button), label) {
					t.Fatalf("button %q is not rendered inside its hitbox", label)
				}
			}
			clickRect(t, m, m.layout.buttons[1])
			if len(b.logCalls) != 1 || b.logCalls[0].previous {
				t.Fatal("visible Logs button missed")
			}
			clickRect(t, m, m.layout.container)
			if m.container != "sidecar" || b.logCalls[len(b.logCalls)-1].container != "sidecar" {
				t.Fatal("container control did not change the log container")
			}
			clickRect(t, m, m.layout.buttons[2])
			if !b.logCalls[len(b.logCalls)-1].previous {
				t.Fatal("visible Previous button missed")
			}
			logCount := len(b.logCalls)
			clickRect(t, m, m.layout.buttons[0])
			if m.inspectorMode != "details" || len(b.logCalls) != logCount {
				t.Fatal("Details button performed a log action")
			}
			listCount := len(b.listCalls)
			clickRect(t, m, m.layout.kindSelector)
			if !m.kindMenu || m.kind != "pods" || len(b.listCalls) != listCount {
				t.Fatal("selector should open choices without changing the current kind")
			}
			clickAt(t, m, m.layout.kindOptions.x+2, m.layout.kindOptions.y+1+2)
			if m.kindMenu || m.kind != "services" || b.listCalls[len(b.listCalls)-1] != "services" {
				t.Fatal("Services menu choice did not refresh services")
			}
		})
	}
}

func clickAt(t *testing.T, m *Model, x, y int) {
	t.Helper()
	_, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	runCommand(t, m, cmd)
}

func clickRect(t *testing.T, m *Model, area rect) {
	t.Helper()
	clickAt(t, m, area.x+area.w/2, area.y+area.h/2)
}

func screenRect(t *testing.T, view string, area rect) string {
	t.Helper()
	lines := strings.Split(view, "\n")
	var result []string
	for y := area.y; y < area.y+area.h; y++ {
		if y < 0 || y >= len(lines) || area.x < 0 || area.x+area.w > lipgloss.Width(lines[y]) {
			t.Fatalf("rectangle %+v extends outside rendered view", area)
		}
		result = append(result, ansi.Strip(ansi.Cut(lines[y], area.x, area.x+area.w)))
	}
	return strings.Join(result, "\n")
}

func TestBlankInspectorAreasAndButtonGapsDoNotTriggerReads(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {140, 44}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, b, _ := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			runCommand(t, m, m.inspect("logs"))
			beforeDetails, beforeLogs, beforeContainer := len(b.detailCalls), len(b.logCalls), m.container
			l := m.layout
			points := [][2]int{
				{l.buttons[0].x - 1, l.buttons[0].y + l.buttons[0].h/2},
				{l.buttons[0].x + l.buttons[0].w, l.buttons[0].y + l.buttons[0].h/2},
				{l.buttons[1].x + l.buttons[1].w, l.buttons[1].y + l.buttons[1].h/2},
				{l.buttons[2].x + l.buttons[2].w, l.buttons[2].y + l.buttons[2].h/2},
				{l.detail.x + 1, l.detail.y + l.detail.h - 1},
				{l.inspector.x + 1, l.inspector.y + 1},
				{l.inspector.x + l.inspector.w - 1, l.inspector.y + l.inspector.h - 1},
				{m.width - 1, m.height - 1},
			}
			for _, point := range points {
				clickAt(t, m, point[0], point[1])
				if len(b.logCalls) != beforeLogs || len(b.detailCalls) != beforeDetails || m.container != beforeContainer {
					t.Fatalf("blank area at %v unexpectedly inspected or changed container", point)
				}
			}
			button := l.buttons[1]
			for _, msg := range []tea.MouseMsg{
				{X: button.x + 1, Y: button.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease},
				{X: button.x + 1, Y: button.y, Button: tea.MouseButtonRight, Action: tea.MouseActionPress},
			} {
				_, cmd := m.Update(msg)
				runCommand(t, m, cmd)
			}
			if len(b.logCalls) != beforeLogs || len(b.detailCalls) != beforeDetails {
				t.Fatal("non-left-press mouse input activated a button")
			}
		})
	}
}

func TestPromptIsBorderedAndClickFocusKeepsShortcutsAsText(t *testing.T) {
	for _, size := range [][2]int{{65, 22}, {80, 24}, {140, 44}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, b, _ := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			box := strings.Split(screenRect(t, m.View(), m.layout.prompt), "\n")
			if len(box) != 3 || !strings.HasPrefix(box[0], "╭") || !strings.HasSuffix(box[0], "╮") ||
				!strings.HasPrefix(box[2], "╰") || !strings.HasSuffix(box[2], "╯") || !strings.Contains(box[1], "›") {
				t.Fatalf("question input lost its separate border: %q", box)
			}
			press(m, "esc")
			// Focus is synchronous; the returned cursor-blink timer need not run.
			m.Update(tea.MouseMsg{X: m.layout.prompt.x + 2, Y: m.layout.prompt.y + 1,
				Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			if m.focus != promptPane || !m.prompt.Focused() {
				t.Fatal("clicking the rendered input did not focus the prompt")
			}
			press(m, "l")
			press(m, "p")
			if m.prompt.Value() != "lp" || len(b.logCalls) != 0 {
				t.Fatal("click-focused question field consumed shortcut letters")
			}
		})
	}
}

func TestResourceMenuChoicesKeyboardAndDismissal(t *testing.T) {
	m, b, _ := testModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	for i, kind := range resourceKinds {
		clickRect(t, m, m.layout.kindSelector)
		menu := screenRect(t, m.View(), m.layout.kindOptions)
		if !strings.Contains(menu, kindLabel(kind)) {
			t.Fatalf("menu is missing %q", kindLabel(kind))
		}
		clickAt(t, m, m.layout.kindOptions.x+2, m.layout.kindOptions.y+1+i)
		if m.kindMenu || m.kind != kind {
			t.Fatalf("menu choice %d selected %q, want %q", i, m.kind, kind)
		}
	}
	m.kind = "pods"
	clickRect(t, m, m.layout.kindSelector)
	press(m, "down")
	runCommand(t, m, press(m, "enter"))
	if m.kindMenu || m.kind != "deployments" {
		t.Fatal("menu arrow/Enter navigation did not choose Deployments")
	}
	before := len(b.listCalls)
	clickRect(t, m, m.layout.kindSelector)
	press(m, "down")
	press(m, "esc")
	if m.kindMenu || m.kind != "deployments" || len(b.listCalls) != before {
		t.Fatal("Escape changed the kind instead of dismissing the menu")
	}
	clickRect(t, m, m.layout.kindSelector)
	clickAt(t, m, 0, 0)
	if m.kindMenu || m.kind != "deployments" || len(b.listCalls) != before {
		t.Fatal("outside click changed the kind instead of dismissing the menu")
	}
}

func TestSelectedResourceStaysVisibleWhilePaging(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {110, 32}, {140, 44}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, _, _ := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			resources := make([]core.Resource, 20)
			for i := range resources {
				resources[i] = fakePod(fmt.Sprintf("pod-%02d", i), "app")
			}
			m.setResources(resources)
			for i := 0; i < len(resources); i++ {
				if !strings.Contains(ansi.Strip(m.resourceTable()), m.selected.Name) {
					t.Fatalf("selected resource %q disappeared at row %d (top %d, visible %d)", m.selected.Name, m.cursor, m.tableTop, m.layout.resourceRows.h)
				}
				m.moveCursor(1)
			}
		})
	}
}

func TestLayoutDimensionsAndFooter(t *testing.T) {
	m, _, _ := testModel(t)
	for _, size := range [][2]int{{80, 24}, {109, 32}, {110, 31}, {110, 32}, {120, 36}, {140, 44}, {65, 22}, {65, 18}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := m.View()
			if lipgloss.Width(view) != size[0] || lipgloss.Height(view) != size[1] {
				t.Fatalf("view measured %dx%d", lipgloss.Width(view), lipgloss.Height(view))
			}
			plain := ansi.Strip(view)
			if size[1] >= 22 {
				for _, label := range []string{"ASK YOUR CLUSTER", "INSPECT", "READ ONLY", "DEMO", "Ctrl+Q Quit"} {
					if !strings.Contains(plain, label) {
						t.Fatalf("missing %q at %v", label, size)
					}
				}
				if m.layout.roomy != (size[0] >= 110 && size[1] >= 32) {
					t.Fatalf("unexpected compact/roomy layout at %v", size)
				}
				for _, area := range []rect{m.layout.prompt, m.layout.resourceRows, m.layout.kindSelector, m.layout.buttons[0], m.layout.buttons[1], m.layout.buttons[2]} {
					screenRect(t, view, area)
				}
			} else if !strings.Contains(plain, "Resize") {
				t.Fatal("small terminal guidance missing")
			}
		})
	}
}

func TestDemoSnapshot(t *testing.T) {
	path := os.Getenv("KUBIX_SNAPSHOT_PATH")
	if path != "" {
		previous := lipgloss.ColorProfile()
		lipgloss.SetColorProfile(termenv.TrueColor)
		defer lipgloss.SetColorProfile(previous)
	}
	backend := demo.New()
	investigator := agent.New(backend, config.DefaultModelSettings(), true)
	m := New(context.Background(), backend, investigator, true)
	defer m.Close()
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	runCommand(t, m, m.refresh())
	m.prompt.SetValue("Why is checkout restarting?")
	runCommand(t, m, press(m, "enter"))
	press(m, "esc")
	runCommand(t, m, press(m, "p"))
	if !strings.Contains(m.detailText, "DATABASE_URL") {
		t.Fatalf("demo logs missing: %s", m.detailText)
	}
	if path != "" {
		if err := os.WriteFile(path, []byte(m.View()), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSelectionBackgroundCoversResourceText(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	m, _, _ := testModel(t)
	row := strings.Split(m.resourceTable(), "\n")[1]
	// Track background changes as a terminal would, so foreground-only cell
	// colors cannot silently clear the highlight around the resource name.
	sgr := regexp.MustCompile(`\x1b\[([0-9;]*)m`)
	background := ""
	position := 0
	for _, match := range sgr.FindAllStringSubmatchIndex(row, -1) {
		text := row[position:match[0]]
		if text != "" && background != "30;75;96" {
			t.Fatalf("selected row segment %q lost its background: %q", text, background)
		}
		params := strings.Split(row[match[2]:match[3]], ";")
		for i := 0; i < len(params); i++ {
			switch params[i] {
			case "", "0", "49":
				background = ""
			case "38", "48":
				color := params[i]
				if i+4 < len(params) && params[i+1] == "2" {
					if color == "48" {
						background = strings.Join(params[i+2:i+5], ";")
					}
					i += 4
				} else if i+2 < len(params) && params[i+1] == "5" {
					if color == "48" {
						background = "indexed:" + params[i+2]
					}
					i += 2
				}
			}
		}
		position = match[1]
	}
	if trailing := row[position:]; trailing != "" && background != "30;75;96" {
		t.Fatalf("selected row trailing segment %q lost its background", trailing)
	}
	if !strings.Contains(ansi.Strip(row), "checkout-abc") {
		t.Fatal("selected resource name missing from row")
	}
}

func TestASCIIColorProfileAppliesToMarkdownAndWorkspace(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(previous)
	m, _, _ := testModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	m.turns = []turn{{question: "Explain the error", answer: "## Diagnosis\n\nThe `checkout` pod is in **CrashLoopBackOff**."}}
	m.renderConversation(true)
	view := m.View()
	// NO_COLOR still permits emphasis (bold/reset); color selections must not
	// escape from Glamour into an otherwise uncolored workspace.
	for _, sequence := range regexp.MustCompile(`\x1b\[([0-9;]*)m`).FindAllStringSubmatch(view, -1) {
		for _, parameter := range strings.Split(sequence[1], ";") {
			code, _ := strconv.Atoi(parameter)
			if (code >= 30 && code <= 49) || (code >= 90 && code <= 107) {
				t.Fatalf("ASCII workspace contains an ANSI color sequence: %q", sequence[0])
			}
		}
	}
	if !strings.Contains(view, "Diagnosis") || !strings.Contains(view, "checkout") {
		t.Fatal("ASCII rendering lost the investigation content")
	}
}
