package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"kubix/internal/redact"
)

var (
	background = lipgloss.Color("#08111e")
	headerBG   = lipgloss.Color("#102033")
	textColor  = lipgloss.Color("#d8e3f0")
	muted      = lipgloss.Color("#95acc3")
	cyan       = lipgloss.Color("#67e8f9")
	lime       = lipgloss.Color("#b6ed78")
	border     = lipgloss.Color("#25425b")
	tableBG    = lipgloss.Color("#0b1726")
	selectedBG = lipgloss.Color("#1e4b60")
	inputBG    = lipgloss.Color("#0e1a29")
	cyanStyle  = lipgloss.NewStyle().Foreground(cyan).Bold(true)
	limeStyle  = lipgloss.NewStyle().Foreground(lime)
	mutedStyle = lipgloss.NewStyle().Foreground(muted)
)

func (m *Model) renderConversation(bottom bool) {
	intro := "## What are you looking for?\n\nTry **Find unhealthy pods**, **Show checkout deployments**, or **Why is this pod restarting?**\n\nDiscover resources through the conversation, then inspect details and logs on the right. Select a resource to give your next question context."
	chunks := []string{m.renderMarkdown(intro)}
	m.citationAnchors = nil
	lineOffset := strings.Count(chunks[0], "\n") + 1
	latestTurnY := 0
	for index, turn := range m.turns {
		latestTurnY = lineOffset + 1
		questionLines := strings.Split(ansi.Hardwrap(turn.question, max(1, m.conversation.Width-2), true), "\n")
		for i, line := range questionLines {
			questionLines[i] = lipgloss.NewStyle().Foreground(lipgloss.Color("#6f9c49")).Render("▌") + limeStyle.Render(" "+line)
		}
		question := strings.Join(questionLines, "\n")
		lineOffset += strings.Count(question, "\n") + 3
		answer := m.renderMarkdown(turn.answer)
		answer = m.anchorCitations(answer, lineOffset, index, turn.investigation)
		chunks = append(chunks, question, answer)
		lineOffset += strings.Count(answer, "\n") + 1
	}
	m.conversation.SetContent(strings.Join(chunks, "\n\n"))
	if bottom {
		if len(m.turns) > 0 && m.turns[len(m.turns)-1].investigation != nil {
			m.conversation.SetYOffset(latestTurnY)
		} else {
			m.conversation.GotoBottom()
		}
	}
}

func (m *Model) renderMarkdown(text string) string {
	text = redact.Text(text, 32000)
	if m.markdown != nil {
		if rendered, err := m.markdown.Render(text); err == nil {
			return strings.Trim(rendered, "\n")
		}
	}
	return ansi.Hardwrap(text, m.conversation.Width, true)
}

func (m *Model) setDetail(text string) {
	m.setFormattedDetail(redact.Text(text, 32000))
}

// Detail commands redact their payload before formatting it for display.
func (m *Model) setFormattedDetail(text string) {
	m.detailText = redact.Sanitize(text, 32000)
	m.inspector.SetContent(ansi.Hardwrap(m.detailText, m.inspector.Width, true))
	m.inspector.GotoTop()
}

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	if m.width < 65 || m.height < 22 {
		return fitBlock("KUBIX\nResize the terminal to at least 65 × 22.\nCtrl+Q to quit.", m.width, m.height)
	}
	lines := m.header()
	if m.layout.roomy {
		lines = append(lines, strings.Repeat(" ", m.width))
	}
	if m.help {
		help := "EXPLORE WITH KUBIX\n\nAsk a question to discover resources. Follow-ups include the selected resource.\n\n/             Focus the question field\nEsc           Focus resources\nTab           Next pane (question, resources, logs, conversation)\n↑ / ↓         Select a resource or scroll the focused pane\ni             Investigate the selected pod and its logs\ne             Choose captured evidence from an earlier answer\nEnter / d     Live resource details\nl / p         Live current / previous pod logs\nc             Select / cycle pod containers\nk             Cycle resource kinds and AI discoveries\nCtrl+R        Refresh the displayed resources\n? / Esc       Close this help\nCtrl+Q        Quit\n\nClick a citation such as [E1] to inspect its original captured data.\nCaptured evidence stays fixed; d/l/p opens live data for the selected row.\nThe mouse wheel scrolls the pane under the pointer."
		lines = append(lines, " "+strings.ReplaceAll(frame(pad(ansi.Hardwrap(help, m.width-8, true), m.width-4), m.width-2, m.bodyHeight, true), "\n", "\n ")+" ")
	} else {
		left := m.conversationPanel()
		right := m.resourcesPanel() + "\n" + strings.Repeat(" ", m.rightWidth) + "\n" + m.inspectorPanel()
		body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
		for _, line := range strings.Split(body, "\n") {
			lines = append(lines, " "+line+" ")
		}
	}
	if m.layout.roomy {
		lines = append(lines, strings.Repeat(" ", m.width))
	}
	lines = append(lines, m.footer())
	view := fitBlock(strings.Join(lines, "\n"), m.width, m.height)
	if m.kindMenu {
		view = overlay(view, m.kindMenuView(), m.layout.kindOptions)
	}
	if m.evidenceMenu {
		view = overlay(view, m.evidenceMenuView(), m.evidenceRect())
	}
	return paint(view, background)
}

func (m *Model) header() []string {
	namespace := m.backend.Namespace()
	if namespace == "" {
		namespace = "all namespaces"
	}
	scope := oneLine(m.backend.ContextName()+"  /  "+namespace, 300)
	brand := cyanStyle.Render("◈  KUBIX")
	mode := limeStyle.Render(oneLine(m.investigator.ModeLabel(), 120))
	var lines []string
	if m.layout.roomy {
		lines = []string{"", "  " + fitLine(brand, 17) + alignEnds(scope, mode, m.width-21) + "  ", "  " + strings.Repeat(" ", 17) + mutedStyle.Render("READ ONLY · Kubernetes explorer")}
	} else {
		lines = []string{"  " + brand + "  " + scope, "  " + alignEnds(mutedStyle.Render("READ ONLY · Kubernetes explorer"), mode, m.width-4)}
	}
	for i, line := range lines {
		lines[i] = surface(line, m.width, headerBG)
	}
	if m.demo {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#e4ea89")).Background(lipgloss.Color("#31331a")).Render(center("DEMO · synthetic resources and logs · no cluster connection", m.width)))
	}
	return lines
}

func (m *Model) conversationPanel() string {
	w := m.leftWidth - 2
	parts := []string{pad(cyanStyle.Render("ASK YOUR CLUSTER"), w), "", pad(viewportWithScrollbar(m.conversation), w), pad(mutedStyle.Render(oneLine(m.activity, 250)), w), ""}
	edge := lipgloss.Color("#36516b")
	if m.focus == promptPane {
		edge = cyan
	}
	box := panel(" "+m.prompt.View(), m.layout.prompt.w, 3, edge, inputBG)
	parts = append(parts, pad(box, w), "")
	return frame(strings.Join(parts, "\n"), m.leftWidth, m.bodyHeight, m.focus == conversationPane)
}

func kindLabel(kind string) string {
	switch kind {
	case "pods":
		return "Pods"
	case "deployments":
		return "Deployments"
	case "services":
		return "Services"
	case "endpointslices":
		return "EndpointSlices"
	case "events":
		return "Events"
	case "nodes":
		return "Nodes"
	default:
		return "AI discoveries"
	}
}

func (m *Model) resourcesPanel() string {
	w := m.rightWidth - 2
	title := "RESOURCES"
	if m.kind == "discovered" {
		title = "DISCOVERED"
	}
	selector := surface(" "+alignEnds(kindLabel(m.kind), "▾ ", m.layout.kindSelector.w-1), m.layout.kindSelector.w, headerBG)
	toolbar := " " + alignEnds(cyanStyle.Render(title), selector, w-2) + " "
	if m.layout.roomy {
		toolbar = strings.Repeat(" ", w) + "\n" + toolbar + "\n" + strings.Repeat(" ", w)
	}
	content := toolbar + "\n" + m.resourceTable() + "\n" + pad(mutedStyle.Render(m.listNotice), w)
	return frame(content, m.rightWidth, m.resourceHeight, m.focus == resourcesPane)
}

func (m *Model) resourceTable() string {
	columns := m.table.Columns()
	rowText := func(cells []string) string {
		var out strings.Builder
		for i, col := range columns {
			out.WriteString(" " + fitLine(ansi.Truncate(cells[i], col.Width, "…"), col.Width) + " ")
		}
		return fitLine(out.String(), m.layout.resourceRows.w)
	}
	headings := make([]string, len(columns))
	for i, col := range columns {
		headings[i] = col.Title
	}
	lines := []string{lipgloss.NewStyle().Foreground(lipgloss.Color("#9eb4c9")).Background(lipgloss.Color("#14283d")).Bold(true).Render(rowText(headings))}
	for i := 0; i < m.layout.resourceRows.h; i++ {
		index := m.tableTop + i
		bg := tableBG
		if index%2 == 1 {
			bg = lipgloss.Color("#0f1c2c")
		}
		style := lipgloss.NewStyle().Foreground(textColor).Background(bg)
		text := strings.Repeat(" ", m.layout.resourceRows.w)
		if index < len(m.resources) {
			r := m.resources[index]
			ns := r.Ref.Namespace
			if ns == "" {
				ns = "—"
			}
			text = rowText([]string{oneLine(r.Ref.Kind, 80), oneLine(ns, 80), oneLine(r.Ref.Name, 253), oneLine(r.Status, 100)})
			if index == m.cursor {
				style = style.Foreground(lipgloss.Color("#e8fbff")).Background(selectedBG).Bold(true)
			}
		} else {
			style = style.Background(tableBG)
		}
		lines = append(lines, style.Render(text))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) inspectorPanel() string {
	w := m.rightWidth - 2
	selection := "Select a resource and press Enter"
	namespace := ""
	if m.selected != nil {
		selection = oneLine(m.selected.Kind+"/"+m.selected.Name, 300)
		namespace = oneLine(m.selected.Namespace, 100)
	}
	title := "INSPECT · LIVE"
	if m.captured != nil {
		title = "INSPECT · CAPTURED [" + oneLine(m.captured.citation.ID, 30) + "]"
		selection = oneLine(m.captured.observation.Ref.DisplayName(), 300)
		namespace = "Snapshot · " + m.captured.observation.CapturedAt.UTC().Format("15:04:05 UTC")
	}
	parts := []string{pad(cyanStyle.Render(title), w), pad(limeStyle.Render(selection), w)}
	if m.layout.selectionHeight == 2 {
		parts = append(parts, pad(mutedStyle.Render(namespace), w))
	}
	labels := []string{"Details", "Logs", "Previous"}
	modes := []string{"details", "logs", "previous"}
	buttons := make([]string, 3)
	for i, label := range labels {
		bg, fg := headerBG, textColor
		if m.inspectorMode == modes[i] {
			bg, fg = selectedBG, cyan
		}
		rows := []string{center(label, m.layout.buttons[i].w)}
		if m.layout.buttonHeight == 3 {
			rows = []string{strings.Repeat(" ", m.layout.buttons[i].w), rows[0], strings.Repeat(" ", m.layout.buttons[i].w)}
		}
		buttons[i] = lipgloss.NewStyle().Foreground(fg).Background(bg).Bold(m.inspectorMode == modes[i]).Render(strings.Join(rows, "\n"))
	}
	parts = append(parts, pad(lipgloss.JoinHorizontal(lipgloss.Top, buttons[0], " ", buttons[1], " ", buttons[2]), w))
	container := ""
	if len(m.containers) > 1 {
		container = surface(mutedStyle.Render("Container: ")+oneLine(m.container, 100)+mutedStyle.Render("  ▸ c"), m.layout.container.w, headerBG)
	}
	if m.captured != nil {
		container = mutedStyle.Render("Snapshot fixed · d/l/p opens live selection · e evidence")
	}
	parts = append(parts, pad(container, w), pad(viewportWithScrollbar(m.inspector), w))
	return frame(strings.Join(parts, "\n"), m.rightWidth, m.inspectorHeight, m.focus == inspectorPane)
}

func viewportWithScrollbar(v viewport.Model) string {
	lines := strings.Split(v.View(), "\n")
	total := v.TotalLineCount()
	thumb, top := v.Height, 0
	if total > v.Height {
		thumb = max(1, v.Height*v.Height/total)
		top = min(v.Height-thumb, v.YOffset*(v.Height-thumb)/max(1, total-v.Height))
	}
	for i, line := range lines {
		bar := " "
		if total > v.Height {
			bar = lipgloss.NewStyle().Foreground(border).Render("│")
			if i >= top && i < top+thumb {
				bar = lipgloss.NewStyle().Foreground(lipgloss.Color("#365b73")).Render("┃")
			}
		}
		lines[i] = fitLine(line, v.Width) + " " + bar
	}
	return strings.Join(lines, "\n")
}

func (m *Model) footer() string {
	items := [][2]string{{"Ctrl+Q", "Quit"}, {"/", "Ask"}, {"i", "Investigate"}, {"e", "Evidence"}, {"d", "Details"}, {"l", "Logs"}, {"p", "Previous"}, {"Ctrl+R", "Refresh"}, {"?", "Help"}}
	if m.width < 110 {
		items = [][2]string{{"Ctrl+Q", "Quit"}, {"/", "Ask"}, {"i", "Investigate"}, {"e", "Evidence"}, {"?", "Help"}}
	}
	var chunks []string
	for _, item := range items {
		chunks = append(chunks, lipgloss.NewStyle().Foreground(lipgloss.Color("#e3b66c")).Bold(true).Render(item[0])+" "+mutedStyle.Render(item[1]))
	}
	return surface(" "+strings.Join(chunks, "  "), m.width, headerBG)
}

func frame(content string, width, height int, focused bool) string {
	edge := border
	if focused {
		edge = cyan
	}
	return panel(content, width, height, edge, background)
}

func panel(content string, width, height int, edgeColor, bg lipgloss.Color) string {
	edge := lipgloss.NewStyle().Foreground(edgeColor)
	inner := max(0, width-2)
	lines := []string{edge.Render("╭" + strings.Repeat("─", inner) + "╮")}
	contentLines := strings.Split(content, "\n")
	for i := 0; i < max(0, height-2); i++ {
		line := ""
		if i < len(contentLines) {
			line = contentLines[i]
		}
		lines = append(lines, edge.Render("│")+fitLine(line, inner)+edge.Render("│"))
	}
	lines = append(lines, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return paint(strings.Join(lines, "\n"), bg)
}

func pad(content string, width int) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = " " + fitLine(line, max(0, width-2)) + " "
	}
	return strings.Join(lines, "\n")
}

func surface(text string, width int, bg lipgloss.Color) string {
	return paint(fitLine(text, width), bg)
}

// Nested Lip Gloss styles reset terminal colors when they close. Restore the
// containing surface after each reset so padded headers and inputs stay filled.
func paint(content string, bg lipgloss.Color) string {
	profile := lipgloss.ColorProfile()
	if profile == termenv.Ascii {
		return content
	}
	base := "\x1b[" + profile.Convert(termenv.RGBColor(textColor)).Sequence(false) + ";" + profile.Convert(termenv.RGBColor(bg)).Sequence(true) + "m"
	content = strings.ReplaceAll(content, "\x1b[0m", "\x1b[0m"+base)
	content = strings.ReplaceAll(content, "\x1b[m", "\x1b[m"+base)
	return base + content + "\x1b[0m"
}

func center(text string, width int) string {
	text = ansi.Truncate(text, width, "")
	return fitLine(strings.Repeat(" ", max(0, (width-lipgloss.Width(text))/2))+text, width)
}

func alignEnds(left, right string, width int) string {
	left = ansi.Truncate(left, max(0, width-lipgloss.Width(right)-1), "…")
	return left + strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(right))) + right
}

func fitLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	line = ansi.Truncate(line, width, "")
	return line + strings.Repeat(" ", max(0, width-lipgloss.Width(line)))
}

func fitBlock(content string, width, height int) string {
	lines := strings.Split(content, "\n")
	result := make([]string, max(0, height))
	for i := range result {
		if i < len(lines) {
			result[i] = fitLine(lines[i], width)
		} else {
			result[i] = strings.Repeat(" ", max(0, width))
		}
	}
	return strings.Join(result, "\n")
}

func overlay(base, content string, area rect) string {
	lines, over := strings.Split(base, "\n"), strings.Split(content, "\n")
	for i, line := range over {
		y := area.y + i
		if y < 0 || y >= len(lines) {
			continue
		}
		lines[y] = ansi.Cut(lines[y], 0, area.x) + fitLine(line, area.w) + ansi.Cut(lines[y], area.x+area.w, lipgloss.Width(lines[y]))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) openKindMenu() {
	m.kindMenu = true
	for i, kind := range resourceKinds {
		if kind == m.kind {
			m.kindChoice = i
		}
	}
}

func (m *Model) chooseKind() tea.Cmd {
	m.kindMenu = false
	m.kind = resourceKinds[m.kindChoice]
	return m.refresh()
}

func (m *Model) kindMenuView() string {
	rows := make([]string, len(resourceKinds))
	for i, kind := range resourceKinds {
		style := lipgloss.NewStyle().Foreground(textColor).Background(headerBG)
		if i == m.kindChoice {
			style = style.Foreground(cyan).Background(selectedBG).Bold(true)
		}
		rows[i] = style.Render(fitLine(" "+kindLabel(kind), m.layout.kindOptions.w-2))
	}
	return panel(strings.Join(rows, "\n"), m.layout.kindOptions.w, m.layout.kindOptions.h, cyan, headerBG)
}

func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	l := m.layout
	wheel := msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown
	if m.evidenceMenu {
		m.evidenceMouse(msg)
		return nil
	}
	if m.kindMenu {
		if wheel {
			delta := 1
			if msg.Button == tea.MouseButtonWheelUp {
				delta = -1
			}
			m.kindChoice = (m.kindChoice + delta + len(resourceKinds)) % len(resourceKinds)
		} else if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
			row := msg.Y - l.kindOptions.y - 1
			if l.kindOptions.contains(msg.X, msg.Y) && row >= 0 && row < len(resourceKinds) {
				m.kindChoice = row
				return m.chooseKind()
			}
			m.kindMenu = false
		}
		return nil
	}
	if wheel {
		delta := 3
		if msg.Button == tea.MouseButtonWheelUp {
			delta = -3
		}
		if l.left.contains(msg.X, msg.Y) {
			m.setFocus(conversationPane)
			if delta < 0 {
				m.conversation.ScrollUp(-delta)
			} else {
				m.conversation.ScrollDown(delta)
			}
		} else if l.resources.contains(msg.X, msg.Y) {
			m.setFocus(resourcesPane)
			m.moveCursor(delta)
		} else if l.inspector.contains(msg.X, msg.Y) {
			m.setFocus(inspectorPane)
			if delta < 0 {
				m.inspector.ScrollUp(-delta)
			} else {
				m.inspector.ScrollDown(delta)
			}
		}
		return nil
	}
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return nil
	}
	if l.prompt.contains(msg.X, msg.Y) {
		return m.setFocus(promptPane)
	}
	if l.left.contains(msg.X, msg.Y) {
		if m.clickCitation(msg.X, msg.Y) {
			return nil
		}
		return m.setFocus(conversationPane)
	}
	if l.resources.contains(msg.X, msg.Y) {
		m.setFocus(resourcesPane)
		if l.kindSelector.contains(msg.X, msg.Y) {
			m.openKindMenu()
			return nil
		}
		if l.resourceRows.contains(msg.X, msg.Y) {
			row := m.tableTop + msg.Y - l.resourceRows.y
			if row < len(m.resources) {
				m.cursor = row
				m.syncTable()
				m.selectCurrent()
				return m.inspect("details")
			}
		}
		return nil
	}
	if l.inspector.contains(msg.X, msg.Y) {
		m.setFocus(inspectorPane)
		for i, button := range l.buttons {
			if button.contains(msg.X, msg.Y) {
				return m.inspect([]string{"details", "logs", "previous"}[i])
			}
		}
		if l.container.contains(msg.X, msg.Y) && len(m.containers) > 1 {
			return m.cycleContainer()
		}
	}
	return nil
}

// Snapshot returns the rendered workspace for demo captures.
func (m *Model) Snapshot() string { return m.View() }
