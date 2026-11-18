// Package tui implements Kubix's interactive, read-only terminal workspace.
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"kubix/internal/core"
	"kubix/internal/redact"
)

type pane int

const (
	promptPane pane = iota
	resourcesPane
	inspectorPane
	conversationPane
)

var resourceKinds = []string{"pods", "deployments", "services", "endpointslices", "events", "nodes", "discovered"}

type turn struct {
	question, answer string
	investigation    *core.Investigation
}

// Model owns all UI state. Background commands communicate only through messages.
type Model struct {
	ctx                                                                       context.Context
	cancel                                                                    context.CancelFunc
	backend                                                                   core.Backend
	investigator                                                              core.Investigator
	demo                                                                      bool
	width, height                                                             int
	layout                                                                    workspaceLayout
	kindMenu                                                                  bool
	kindChoice                                                                int
	leftWidth, rightWidth, bodyHeight, resourceHeight, inspectorHeight, bodyY int
	focus                                                                     pane
	help, quitting                                                            bool
	prompt                                                                    textinput.Model
	table                                                                     table.Model
	conversation, inspector                                                   viewport.Model
	markdown                                                                  *glamour.TermRenderer
	turns                                                                     []turn
	activity, detailText, listNotice                                          string
	resources                                                                 []core.Resource
	cursor, tableTop                                                          int
	selected                                                                  *core.ResourceRef
	kind                                                                      string
	discovered                                                                []core.ResourceRef
	containers                                                                []string
	container, inspectorMode                                                  string
	containerChosen                                                           bool
	asking                                                                    bool
	askGeneration, listGeneration, detailGeneration                           uint64
	listCancel, detailCancel                                                  context.CancelFunc
	lastAnswer                                                                *core.Answer
	citationAnchors                                                           []citationAnchor
	captured                                                                  *capturedEvidence
	evidenceMenu                                                              bool
	evidenceChoice, evidenceTop                                               int
	evidenceItems                                                             []evidenceItem
}

// New creates the workspace. The caller may run it using tea.NewProgram with
// tea.WithAltScreen and tea.WithMouseCellMotion. No Kubernetes reads happen here.
func New(ctx context.Context, backend core.Backend, investigator core.Investigator, demo bool) *Model {
	ctx, cancel := context.WithCancel(ctx)
	prompt := textinput.New()
	prompt.Placeholder = "Ask about your cluster…"
	prompt.Prompt = "› "
	prompt.CharLimit = 4000
	prompt.PromptStyle = lipgloss.NewStyle().Foreground(cyan).Bold(true)
	prompt.TextStyle = lipgloss.NewStyle().Foreground(textColor)
	prompt.PlaceholderStyle = lipgloss.NewStyle().Foreground(muted)
	prompt.Focus()
	t := table.New(table.WithFocused(true))
	t.SetStyles(table.Styles{
		Header: lipgloss.NewStyle().Foreground(muted).Background(headerBG).Bold(true).Padding(0, 1),
		// Keep cells free of ANSI color resets so Selected can paint the whole row.
		// The table's default foreground is applied around its completed view.
		Cell:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle().Foreground(lipgloss.Color("#e8fbff")).Background(lipgloss.Color("#1e4b60")).Bold(true),
	})
	m := &Model{
		ctx: ctx, cancel: cancel, backend: backend, investigator: investigator, demo: demo,
		prompt: prompt, table: t, kind: "pods", cursor: -1, inspectorMode: "details",
		activity: "Ready to explore", listNotice: "Loading pods…",
		detailText:   "Select a resource and press Enter to inspect it.",
		conversation: viewport.New(40, 20), inspector: viewport.New(60, 15),
	}
	m.resize(120, 36)
	return m
}

func (m *Model) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.refresh()) }

// Close cancels any outstanding Kubernetes or model calls.
func (m *Model) Close() { m.cancel() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case listResultMsg:
		if msg.generation != m.listGeneration {
			return m, nil
		}
		if len(msg.resources) > 0 || len(msg.errors) == 0 {
			m.setResources(msg.resources)
		}
		m.listNotice = fmt.Sprintf("%d resources · Enter details · l logs", len(m.resources))
		if msg.limited {
			m.listNotice = fmt.Sprintf("%d resources · bounded sample; more may exist", len(m.resources))
		}
		if len(msg.errors) > 0 {
			m.listNotice = oneLine(strings.Join(msg.errors, "; "), 500)
		}
		return m, nil
	case detailResultMsg:
		if m.captured != nil || msg.generation != m.detailGeneration || !sameRef(m.selected, &msg.ref) {
			return m, nil
		}
		if m.container != msg.container {
			m.containerChosen = false
		}
		m.containers, m.container = msg.containers, msg.container
		m.setFormattedDetail(msg.text)
		return m, nil
	case progressMsg:
		if msg.generation == m.askGeneration && m.asking {
			m.activity = oneLine(msg.text, 250)
		}
		return m, waitProgress(m.ctx, msg.generation, msg.events)
	case answerMsg:
		if msg.generation != m.askGeneration {
			return m, nil
		}
		m.asking = false
		m.lastAnswer = &msg.answer
		if len(m.turns) > 0 {
			m.turns[len(m.turns)-1].answer = redact.Text(msg.answer.Text, 32000)
			m.turns[len(m.turns)-1].investigation = msg.answer.Investigation.Clone()
		}
		m.activity = fmt.Sprintf("Complete · %d evidence queries", len(msg.answer.Evidence))
		if msg.answer.Investigation != nil {
			m.activity = fmt.Sprintf("Complete · %d captured sources · click citations or e", len(msg.answer.Investigation.Observations))
		}
		if len(msg.answer.Resources) > 0 {
			m.listGeneration++
			if m.listCancel != nil {
				m.listCancel()
			}
			m.kind = "discovered"
			m.setResources(msg.answer.Resources)
			m.discovered = refsFrom(m.resources)
			m.listNotice = fmt.Sprintf("%d discovered resources · Enter details", len(m.resources))
			for _, result := range msg.answer.Evidence {
				if limited(result) {
					m.listNotice = fmt.Sprintf("%d resources · bounded sample; more may exist", len(m.resources))
					break
				}
			}
		}
		m.renderConversation(true)
		return m, nil
	case tea.MouseMsg:
		if !m.help {
			return m, m.mouse(msg)
		}
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+q" || key == "ctrl+c" {
			m.quitting = true
			m.Close()
			return m, tea.Quit
		}
		if m.evidenceMenu {
			m.evidenceKey(key)
			return m, nil
		}
		if m.kindMenu {
			switch key {
			case "esc":
				m.kindMenu = false
			case "up":
				m.kindChoice = (m.kindChoice + len(resourceKinds) - 1) % len(resourceKinds)
			case "down", "j", "k":
				m.kindChoice = (m.kindChoice + 1) % len(resourceKinds)
			case "enter":
				return m, m.chooseKind()
			}
			return m, nil
		}
		if m.help {
			if key == "esc" || key == "?" || key == "enter" {
				m.help = false
			}
			return m, nil
		}
		switch key {
		case "tab":
			return m, m.setFocus((m.focus + 1) % 4)
		case "shift+tab":
			return m, m.setFocus((m.focus + 3) % 4)
		case "esc":
			return m, m.setFocus(resourcesPane)
		case "ctrl+r":
			return m, m.refresh()
		}
		if m.focus == promptPane {
			if key == "enter" {
				return m, m.ask()
			}
			var cmd tea.Cmd
			m.prompt, cmd = m.prompt.Update(msg)
			return m, cmd
		}
		switch key {
		case "/":
			return m, m.setFocus(promptPane)
		case "?":
			m.help = true
			return m, nil
		case "k":
			return m, m.cycleKind()
		case "d", "enter":
			return m, m.inspect("details")
		case "l":
			return m, m.inspect("logs")
		case "p":
			return m, m.inspect("previous")
		case "c":
			return m, m.cycleContainer()
		case "e":
			m.openEvidenceMenu()
			return m, nil
		case "i":
			return m, m.investigateSelected()
		}
		if m.focus == resourcesPane {
			switch key {
			case "up":
				m.moveCursor(-1)
			case "down", "j":
				m.moveCursor(1)
			case "pgup":
				m.moveCursor(-max(1, m.table.Height()))
			case "pgdown":
				m.moveCursor(max(1, m.table.Height()))
			case "home", "g":
				m.moveCursor(-len(m.resources))
			case "end", "G":
				m.moveCursor(len(m.resources))
			}
			return m, nil
		}
		var cmd tea.Cmd
		if m.focus == inspectorPane {
			m.inspector, cmd = m.inspector.Update(msg)
		} else {
			m.conversation, cmd = m.conversation.Update(msg)
		}
		return m, cmd
	}
	// Blink messages still belong to the text input, without stealing focus.
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return m, cmd
}

func (m *Model) setFocus(focus pane) tea.Cmd {
	m.focus = focus
	if focus == promptPane {
		return m.prompt.Focus()
	}
	m.prompt.Blur()
	return nil
}

func (m *Model) ask() tea.Cmd {
	question := strings.TrimSpace(m.prompt.Value())
	if question != "" && !m.asking {
		m.prompt.SetValue("")
	}
	return m.askQuestion(question)
}

func (m *Model) askQuestion(question string) tea.Cmd {
	return m.startQuestion(question, false)
}

func (m *Model) startQuestion(question string, crashLoop bool) tea.Cmd {
	if question == "" {
		return nil
	}
	if m.asking {
		m.activity = "An investigation is running; your next question is kept below."
		return nil
	}
	m.asking = true
	m.askGeneration++
	m.turns = append(m.turns, turn{question: redact.Text(question, 4000), answer: "Investigating…"})
	if len(m.turns) > 20 {
		m.turns = append([]turn(nil), m.turns[len(m.turns)-20:]...)
	}
	m.activity = "Gathering evidence…"
	m.renderConversation(true)
	var selected *core.ResourceRef
	if m.selected != nil {
		copied := *m.selected
		selected = &copied
	}
	events := make(chan string, 16)
	if crashLoop && selected != nil {
		container := ""
		if m.containerChosen {
			container = m.container
		}
		return tea.Batch(crashLoopCommand(m.ctx, m.investigator, m.askGeneration, question, *selected, container, events), waitProgress(m.ctx, m.askGeneration, events))
	}
	return tea.Batch(askCommand(m.ctx, m.investigator, m.askGeneration, question, selected, events), waitProgress(m.ctx, m.askGeneration, events))
}

func (m *Model) refresh() tea.Cmd {
	if m.listCancel != nil {
		m.listCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.listCancel = cancel
	m.listGeneration++
	m.listNotice = "Refreshing resources…"
	var refs []core.ResourceRef
	if m.kind == "discovered" {
		refs = append([]core.ResourceRef{}, m.discovered...)
	}
	return listCommand(ctx, m.backend, m.listGeneration, m.kind, m.backend.Namespace(), refs)
}

func (m *Model) cycleKind() tea.Cmd {
	index := 0
	for i, kind := range resourceKinds {
		if kind == m.kind {
			index = i
			break
		}
	}
	index = (index + 1) % len(resourceKinds)
	if resourceKinds[index] == "discovered" && len(m.discovered) == 0 {
		index = 0
	}
	m.kind = resourceKinds[index]
	return m.refresh()
}

func (m *Model) inspect(mode string) tea.Cmd {
	m.captured = nil
	m.inspectorMode = mode
	if m.selected == nil {
		m.setDetail("Select a resource first.")
		return nil
	}
	if mode != "details" && m.selected.Kind != "pods" {
		m.setDetail("Logs are available for pods. Ask Kubix to find this resource's pods.")
		return nil
	}
	if m.detailCancel != nil {
		m.detailCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.detailCancel = cancel
	m.detailGeneration++
	m.inspectorMode = mode
	m.setDetail("Loading " + mode + " for " + m.selected.Name + "…")
	return detailCommand(ctx, m.backend, m.detailGeneration, *m.selected, mode, m.container)
}

func (m *Model) cycleContainer() tea.Cmd {
	if m.captured != nil {
		m.activity = "Captured evidence stays fixed. Open live details or logs to choose a container."
		return nil
	}
	if len(m.containers) == 0 {
		m.activity = "Open Pod details before selecting a container."
		return nil
	}
	if len(m.containers) == 1 {
		m.container = m.containers[0]
		m.containerChosen = true
		m.activity = "Selected container " + m.container + " for investigation."
		return nil
	}
	if len(m.containers) < 2 {
		return nil
	}
	index := 0
	for i, name := range m.containers {
		if name == m.container {
			index = (i + 1) % len(m.containers)
			break
		}
	}
	m.container = m.containers[index]
	m.containerChosen = true
	if m.inspectorMode != "details" {
		return m.inspect(m.inspectorMode)
	}
	return nil
}

func (m *Model) setResources(resources []core.Resource) {
	selectedKey := ""
	if m.selected != nil {
		selectedKey = m.selected.Key()
	}
	m.resources = nil
	seen := map[string]bool{}
	m.cursor = 0
	for _, resource := range resources {
		key := resource.Ref.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		if key == selectedKey {
			m.cursor = len(m.resources)
		}
		m.resources = append(m.resources, resource)
		if len(m.resources) == 100 {
			break
		}
	}
	m.tableTop = 0
	if len(m.resources) == 0 {
		m.cursor = -1
	}
	m.syncTable()
	m.selectCurrent()
}

func (m *Model) moveCursor(delta int) {
	if len(m.resources) == 0 {
		return
	}
	m.cursor = max(0, min(len(m.resources)-1, m.cursor+delta))
	m.syncTable()
	m.selectCurrent()
}

func (m *Model) selectCurrent() {
	var selected *core.ResourceRef
	if m.cursor >= 0 && m.cursor < len(m.resources) {
		ref := m.resources[m.cursor].Ref
		selected = &ref
	}
	if sameRef(m.selected, selected) {
		return
	}
	m.selected = selected
	m.detailGeneration++
	if m.detailCancel != nil {
		m.detailCancel()
	}
	m.containers, m.container = nil, ""
	m.containerChosen = false
	if m.captured != nil {
		return
	}
	m.inspectorMode = "details"
	if selected == nil {
		m.setDetail("No resources to inspect.")
	} else {
		m.setDetail("Press Enter to inspect this resource.")
	}
}

func (m *Model) syncTable() {
	count := max(1, m.table.Height())
	if m.cursor < m.tableTop {
		m.tableTop = max(0, m.cursor)
	}
	if m.cursor >= m.tableTop+count {
		m.tableTop = m.cursor - count + 1
	}
	m.tableTop = max(0, min(m.tableTop, max(0, len(m.resources)-count)))
	rows := []table.Row{}
	for i := m.tableTop; i < min(len(m.resources), m.tableTop+count); i++ {
		r := m.resources[i]
		namespace := r.Ref.Namespace
		if namespace == "" {
			namespace = "—"
		}
		rows = append(rows, table.Row{oneLine(r.Ref.Kind, 80), oneLine(namespace, 80), oneLine(r.Ref.Name, 253), oneLine(r.Status, 100)})
	}
	m.table.SetRows(rows)
	if len(rows) > 0 {
		m.table.SetCursor(m.cursor - m.tableTop)
	}
}

func sameRef(a, b *core.ResourceRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Key() == b.Key()
}

func refsFrom(resources []core.Resource) []core.ResourceRef {
	refs := make([]core.ResourceRef, len(resources))
	for i, r := range resources {
		refs[i] = r.Ref
	}
	return refs
}

func oneLine(value string, limit int) string {
	return strings.ReplaceAll(strings.ReplaceAll(redact.Text(value, limit), "\n", " "), "\t", " ")
}
