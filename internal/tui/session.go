package tui

import (
	"context"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"kubix/internal/core"
	"strings"
)

// ContextChoice is a configured context and its effective namespace scope.
type ContextChoice struct {
	Name, Namespace string
	Current         bool
}

// Session couples the backend and investigator for exactly one context.
type Session struct {
	Backend      core.Backend
	Investigator core.Investigator
}
type SessionOpener func(context.Context, string) (Session, error)
type sessionResultMsg struct {
	generation uint64
	session    Session
	err        error
}

// ConfigureContexts enables in-memory context selection without kubeconfig writes.
func (m *Model) ConfigureContexts(choices []ContextChoice, open SessionOpener) {
	m.contexts = append([]ContextChoice(nil), choices...)
	m.openSession = open
}
func (m *Model) showContexts() {
	if m.openSession == nil || len(m.contexts) == 0 {
		m.activity = "Context switching unavailable in this session"
		return
	}
	m.kindMenu = false
	m.evidenceMenu = false
	m.help = false
	m.contextMenu = true
	for i, c := range m.contexts {
		if c.Current {
			m.contextChoice = i
		}
	}
}
func (m *Model) contextKey(key string) tea.Cmd {
	if key == "esc" {
		m.contextMenu = false
		if m.switching {
			m.switchGeneration++
			m.switchCancel()
			m.switching = false
			m.activity = "Context switch cancelled; previous context retained"
			return m.refresh()
		}
		return nil
	}
	if m.switching {
		return nil
	}
	switch key {
	case "up":
		m.contextChoice = (m.contextChoice + len(m.contexts) - 1) % len(m.contexts)
	case "down":
		m.contextChoice = (m.contextChoice + 1) % len(m.contexts)
	case "enter":
		return m.beginSwitch()
	}
	return nil
}
func (m *Model) clearSessionView() {
	m.cancel()
	m.ctx, m.cancel = context.WithCancel(m.rootCtx)
	m.askGeneration++
	m.listGeneration++
	m.detailGeneration++
	m.asking = false
	m.listLoading = false
	m.resources = nil
	m.selected = nil
	m.cursor = -1
	m.tableTop = 0
	m.table.SetRows(nil)
	m.turns = nil
	m.lastAnswer = nil
	m.discovered = nil
	m.containers = nil
	m.container = ""
	m.containerChosen = false
	m.captured = nil
	m.citationAnchors = nil
	m.evidenceItems = nil
	m.evidenceMenu = false
	m.evidenceChoice = 0
	m.evidenceTop = 0
	m.kindMenu = false
	m.kind = "pods"
	m.kindChoice = 0
	m.inspectorMode = "details"
	m.pageCursor = ""
	m.nextCursor = ""
	m.pageHistory = nil
	m.prompt.SetValue("")
	m.setDetail("Select a resource to inspect it.")
	m.renderConversation(false)
	m.conversation.GotoTop()
}
func (m *Model) beginSwitch() tea.Cmd {
	choice := m.contexts[m.contextChoice]
	m.clearSessionView()
	m.switching = true
	m.switchGeneration++
	ctx, cancel := context.WithCancel(m.rootCtx)
	m.switchCancel = cancel
	generation, open := m.switchGeneration, m.openSession
	m.activity = "Opening context " + oneLine(choice.Name, 100) + "; previous context inactive"
	m.listNotice = "Context switch in progress"
	return func() (msg tea.Msg) {
		defer func() {
			if recover() != nil {
				msg = sessionResultMsg{generation: generation, err: fmt.Errorf("context initialization failed")}
			}
		}()
		session, err := open(ctx, choice.Name)
		if session.Investigator != nil {
			go func() {
				<-ctx.Done()
				if closer, ok := session.Investigator.(interface{ Close() }); ok {
					closer.Close()
				}
			}()
		}
		return sessionResultMsg{generation, session, err}
	}
}
func (m *Model) finishSwitch(msg sessionResultMsg) tea.Cmd {
	if !m.switching || msg.generation != m.switchGeneration {
		return nil
	}
	m.switching = false
	m.contextMenu = false
	if msg.err != nil || msg.session.Backend == nil || msg.session.Investigator == nil {
		m.switchCancel()
		m.switchCancel = nil
		m.activity = "Context switch failed; previous context retained (check configuration and access)"
		return m.refresh()
	}
	if m.activeSessionCancel != nil {
		m.activeSessionCancel()
	}
	m.activeSessionCancel = m.switchCancel
	m.switchCancel = nil
	m.backend = msg.session.Backend
	m.investigator = msg.session.Investigator
	for i := range m.contexts {
		m.contexts[i].Current = i == m.contextChoice
	}
	m.activity = "Context switched; previous conversation and evidence cleared"
	return m.refresh()
}
func (m *Model) contextMenuView() string {
	lines := []string{"CONTEXT / NAMESPACE", "Enter opens · Esc cancels"}
	if m.switching {
		lines = append(lines, "Opening selection; Esc cancels")
	}
	start := max(0, m.contextChoice-2)
	end := min(len(m.contexts), start+5)
	for i := start; i < end; i++ {
		c := m.contexts[i]
		if c.Namespace == "" {
			c.Namespace = "all namespaces"
		}
		prefix := "  "
		if i == m.contextChoice {
			prefix = "> "
		}
		current := ""
		if c.Current {
			current = " (current)"
		}
		lines = append(lines, prefix+oneLine(c.Name, max(10, min(m.width-12, 80)))+current, "  namespace: "+oneLine(c.Namespace, max(10, min(m.width-18, 70))))
	}
	return strings.Join(lines, "\n")
}
func (m *Model) clearPageSelection() {
	if m.detailCancel != nil {
		m.detailCancel()
	}
	m.detailGeneration++
	m.resources = nil
	m.selected = nil
	m.cursor = -1
	m.tableTop = 0
	m.table.SetRows(nil)
	m.captured = nil
	m.containers = nil
	m.container = ""
	m.containerChosen = false
	m.setDetail("Select a resource on this page.")
}
func (m *Model) nextPage() tea.Cmd {
	if m.listLoading || m.nextCursor == "" {
		return nil
	}
	if len(m.pageHistory) >= 100 {
		m.listNotice = "Page history limit reached; Ctrl+R restarts browsing"
		return nil
	}
	m.pageHistory = append(m.pageHistory, m.pageCursor)
	m.pageCursor = m.nextCursor
	m.nextCursor = ""
	m.clearPageSelection()
	return m.loadPage()
}
func (m *Model) previousPage() tea.Cmd {
	if m.listLoading || len(m.pageHistory) == 0 {
		return nil
	}
	n := len(m.pageHistory) - 1
	m.pageCursor = m.pageHistory[n]
	m.pageHistory = m.pageHistory[:n]
	m.nextCursor = ""
	m.clearPageSelection()
	return m.loadPage()
}
