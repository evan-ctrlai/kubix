package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"sigs.k8s.io/yaml"

	"kubix/internal/core"
	"kubix/internal/redact"
)

var evidenceToken = regexp.MustCompile(`\[(E[1-9][0-9]*)\]`)

// Coordinates are cells in the fully rendered conversation, before viewport
// scrolling. Each anchor owns its turn's capture, so E1 never resolves globally.
type citationAnchor struct {
	x, y, width int
	item        evidenceItem
}

type evidenceItem struct {
	investigation *core.Investigation
	citationID    string
	turn          int
}

type capturedEvidence struct {
	item        evidenceItem
	observation core.Observation
	citation    core.EvidenceCitation
}

func (m *Model) anchorCitations(rendered string, offset, turn int, investigation *core.Investigation) string {
	if investigation == nil {
		return rendered
	}
	lines := strings.Split(rendered, "\n")
	for row, line := range lines {
		plain := ansi.Strip(line)
		matches := evidenceToken.FindAllStringSubmatchIndex(plain, -1)
		// Work right to left so adding trusted styles preserves all cell positions.
		for n := len(matches) - 1; n >= 0; n-- {
			match := matches[n]
			id := plain[match[2]:match[3]]
			if _, _, ok := investigation.ResolveCitation(id); !ok {
				continue
			}
			x := ansi.StringWidth(plain[:match[0]])
			width := ansi.StringWidth(plain[match[0]:match[1]])
			m.citationAnchors = append(m.citationAnchors, citationAnchor{x, offset + row, width, evidenceItem{investigation, id, turn}})
			styled := lipgloss.NewStyle().Foreground(cyan).Underline(true).Bold(true).Render(plain[match[0]:match[1]])
			line = ansi.Cut(line, 0, x) + styled + ansi.Cut(line, x+width, ansi.StringWidth(line))
		}
		lines[row] = line
	}
	return strings.Join(lines, "\n")
}

func (m *Model) clickCitation(x, y int) bool {
	area := m.layout.conversation
	if !area.contains(x, y) {
		return false
	}
	column, row := x-area.x, y-area.y+m.conversation.YOffset
	for _, anchor := range m.citationAnchors {
		if row == anchor.y && column >= anchor.x && column < anchor.x+anchor.width {
			return m.openCaptured(anchor.item)
		}
	}
	return false
}

func (m *Model) openCaptured(item evidenceItem) bool {
	observation, citation, ok := item.investigation.ResolveCitation(item.citationID)
	if !ok {
		m.activity = "That citation is not present in the captured evidence."
		return false
	}
	if m.detailCancel != nil {
		m.detailCancel()
	}
	m.detailGeneration++
	m.captured = &capturedEvidence{item, observation, citation}
	m.inspectorMode = "captured"
	m.evidenceMenu = false
	m.setFocus(inspectorPane)
	m.setFormattedDetail(capturedText(item.investigation, observation, citation))
	m.activity = fmt.Sprintf("Captured [%s] · answer %d · no live query", citation.ID, item.turn+1)
	return true
}

func capturedText(investigation *core.Investigation, observation core.Observation, citation core.EvidenceCitation) string {
	capturedAt := "time unavailable"
	if !observation.CapturedAt.IsZero() {
		capturedAt = observation.CapturedAt.UTC().Format(time.RFC3339)
	}
	state := observation.State
	if state == "" {
		state = "completeness unspecified"
	}
	lines := []string{
		fmt.Sprintf("[%s] %s", citation.ID, citation.Label),
		"Captured: " + capturedAt + " · " + state,
		"Source: " + observation.Source,
	}
	if len(observation.Errors) > 0 {
		lines = append(lines, fmt.Sprintf("Evidence gaps: %d · details below", len(observation.Errors)))
	}
	// Metadata is untrusted cluster/model text, never terminal styling.
	text := redact.Text(strings.Join(lines, "\n"), 8000)
	payload := redact.Text(observation.Payload, 16000)
	if observation.Format == "json" {
		var value any
		if json.Unmarshal([]byte(observation.Payload), &value) == nil {
			payload = capturedYAML(value)
		}
	}
	if citation.JSONPointer != "" {
		if value, ok := core.JSONPointerValue(observation.Payload, citation.JSONPointer); ok {
			text += "\n\n▶ CITED FIELD " + redact.Text(citation.JSONPointer, 1000) + "\n" + capturedYAML(value)
		}
	} else if citation.LineStart > 0 {
		lines := strings.Split(strings.TrimSuffix(observation.Payload, "\n"), "\n")
		text += fmt.Sprintf("\n\n▶ CITED LOG LINES %d–%d\n", citation.LineStart, citation.LineEnd)
		// ResolveCitation validates bounds; retain a guard for malformed captures.
		for n := max(1, citation.LineStart); n <= min(citation.LineEnd, len(lines)); n++ {
			text += fmt.Sprintf("▶ %d │ %s\n", n, redact.Text(lines[n-1], 16000))
		}
	} else if payload != "" {
		preview := strings.Split(payload, "\n")
		text += "\n\nCAPTURED PREVIEW\n" + redact.Sanitize(strings.Join(preview[:min(12, len(preview))], "\n"), 6000)
		if len(preview) > 12 {
			text += "\n… first 12 lines; full captured document below"
		}
	}
	metadata := []string{
		"Resource: " + observation.Ref.DisplayName(),
		"UID: " + observation.Ref.UID,
		"Query: " + observation.Query,
		"Context: " + investigation.Context + " · scope: " + investigation.Namespace,
	}
	if observation.ResourceVersion != "" {
		metadata = append(metadata, "Resource version: "+observation.ResourceVersion)
	}
	text += "\n\nCAPTURE METADATA\n" + redact.Text(strings.Join(metadata, "\n"), 8000)
	if len(observation.Notes) > 0 {
		text += "\n\nCoverage / notes:\n- " + redact.Text(strings.Join(observation.Notes, "\n- "), 4000)
	}
	if len(observation.Errors) > 0 {
		text += "\n\nEvidence gaps:\n- " + redact.Text(strings.Join(observation.Errors, "\n- "), 4000)
	}
	if len(investigation.Limitations) > 0 {
		text += "\n\nInvestigation limits:\n- " + redact.Text(strings.Join(investigation.Limitations, "\n- "), 4000)
	}
	text += "\n\nCAPTURED DOCUMENT · fixed at collection time\n"
	text += payload
	return redact.Sanitize(text, 32000)
}

func capturedYAML(value any) string {
	encoded, err := yaml.Marshal(redact.Value(value, 16000))
	if err != nil {
		return "Captured value could not be formatted."
	}
	return redact.Sanitize(string(encoded), 16000)
}

func (m *Model) openEvidenceMenu() {
	m.evidenceItems = nil
	for index := len(m.turns) - 1; index >= 0; index-- {
		capture := m.turns[index].investigation
		if capture == nil {
			continue
		}
		for _, citation := range capture.Citations {
			if _, _, ok := capture.ResolveCitation(citation.ID); ok {
				m.evidenceItems = append(m.evidenceItems, evidenceItem{capture, citation.ID, index})
			}
		}
	}
	if len(m.evidenceItems) == 0 {
		m.activity = "No captured investigation yet. Select a pod and press i to investigate."
		return
	}
	m.kindMenu = false
	m.evidenceMenu = true
	m.evidenceChoice, m.evidenceTop = 0, 0
}

func (m *Model) evidenceRect() rect {
	w := min(96, max(30, m.width-6))
	h := min(max(6, len(m.evidenceItems)+4), max(6, m.height-6))
	return rect{(m.width - w) / 2, (m.height - h) / 2, w, h}
}

func (m *Model) moveEvidence(delta int) {
	m.evidenceChoice = max(0, min(len(m.evidenceItems)-1, m.evidenceChoice+delta))
	rows := m.evidenceRect().h - 4
	if m.evidenceChoice < m.evidenceTop {
		m.evidenceTop = m.evidenceChoice
	}
	if m.evidenceChoice >= m.evidenceTop+rows {
		m.evidenceTop = m.evidenceChoice - rows + 1
	}
}

func (m *Model) evidenceKey(key string) {
	switch key {
	case "esc", "e":
		m.evidenceMenu = false
	case "up", "k":
		m.moveEvidence(-1)
	case "down", "j":
		m.moveEvidence(1)
	case "pgup":
		m.moveEvidence(-(m.evidenceRect().h - 4))
	case "pgdown":
		m.moveEvidence(m.evidenceRect().h - 4)
	case "enter":
		if m.evidenceChoice >= 0 && m.evidenceChoice < len(m.evidenceItems) {
			m.openCaptured(m.evidenceItems[m.evidenceChoice])
		}
	}
}

func (m *Model) evidenceMenuView() string {
	area := m.evidenceRect()
	rows := []string{cyanStyle.Render(" CAPTURED EVIDENCE · newest answer first")}
	for row := 0; row < area.h-4; row++ {
		index := m.evidenceTop + row
		line := ""
		style := lipgloss.NewStyle().Foreground(textColor)
		if index < len(m.evidenceItems) {
			item := m.evidenceItems[index]
			observation, citation, _ := item.investigation.ResolveCitation(item.citationID)
			line = fmt.Sprintf(" %d · [%s] %s · %s", item.turn+1, citation.ID, oneLine(citation.Label, 200), oneLine(observation.State, 50))
			if index == m.evidenceChoice {
				style = style.Foreground(cyan).Background(selectedBG).Bold(true)
			}
		}
		rows = append(rows, style.Render(fitLine(line, area.w-2)))
	}
	rows = append(rows, mutedStyle.Render(fmt.Sprintf(" ↑/↓ choose · Enter inspect · Esc close · %d/%d", m.evidenceChoice+1, len(m.evidenceItems))))
	return panel(strings.Join(rows, "\n"), area.w, area.h, cyan, headerBG)
}

func (m *Model) evidenceMouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.moveEvidence(-1)
	case tea.MouseButtonWheelDown:
		m.moveEvidence(1)
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return
		}
		area := m.evidenceRect()
		row := msg.Y - area.y - 2
		index := m.evidenceTop + row
		if area.contains(msg.X, msg.Y) && row >= 0 && row < area.h-4 && index < len(m.evidenceItems) {
			m.evidenceChoice = index
			m.openCaptured(m.evidenceItems[index])
		} else if !area.contains(msg.X, msg.Y) {
			m.evidenceMenu = false
		}
	}
}

func (m *Model) investigateSelected() tea.Cmd {
	if m.selected == nil || m.selected.Kind != "pods" {
		m.activity = "Select a pod to investigate CrashLoopBackOff; other resources support ordinary questions."
		return nil
	}
	return m.startQuestion("Investigate the selected pod and its current/previous logs", true)
}
