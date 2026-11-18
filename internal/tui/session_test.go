package tui

import (
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"kubix/internal/core"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type closingInvestigator struct {
	*fakeInvestigator
	closes atomic.Int32
	closed chan struct{}
}

func (i *closingInvestigator) Close() {
	if i.closes.Add(1) == 1 {
		close(i.closed)
	}
}
func closingAgent(b *fakeBackend) *closingInvestigator {
	return &closingInvestigator{fakeInvestigator: &fakeInvestigator{backend: b}, closed: make(chan struct{})}
}
func assertClosed(t *testing.T, i *closingInvestigator) {
	t.Helper()
	select {
	case <-i.closed:
	case <-time.After(time.Second):
		t.Fatal("investigator not closed")
	}
	if i.closes.Load() != 1 {
		t.Fatal("duplicate close")
	}
}
func TestContextSwitchClearsStateAndRejectsOldMessages(t *testing.T) {
	b := newFakeBackend()
	old := closingAgent(b)
	m := New(context.Background(), b, old, false)
	defer m.Close()
	oldCtx := m.ctx
	m.setResources(b.resources)
	m.turns = []turn{{question: "old", answer: "old"}}
	m.asking = true
	m.nextCursor = "old-cursor"
	m.pageHistory = []string{""}
	m.evidenceMenu = true
	ag, lg, dg := m.askGeneration, m.listGeneration, m.detailGeneration
	next := newFakeBackend()
	a := closingAgent(next)
	m.ConfigureContexts([]ContextChoice{{Name: "next", Namespace: "other"}}, func(context.Context, string) (Session, error) { return Session{next, a}, nil })
	m.showContexts()
	cmd := m.contextKey("enter")
	if oldCtx.Err() == nil {
		t.Fatal("old requests not canceled")
	}
	if m.selected != nil || len(m.resources) != 0 || len(m.turns) != 0 || m.asking || m.nextCursor != "" || len(m.pageHistory) != 0 || m.evidenceMenu {
		t.Fatal("old state retained")
	}
	m.Update(answerMsg{generation: ag, answer: core.Answer{Text: "stale", Resources: b.resources}})
	m.Update(listResultMsg{generation: lg, resources: b.resources})
	m.Update(detailResultMsg{generation: dg, text: "stale"})
	_, progress := m.Update(progressMsg{generation: ag, text: "stale", events: make(chan string)})
	if len(m.resources) != 0 || m.lastAnswer != nil || progress != nil {
		t.Fatal("stale response accepted")
	}
	msg := cmd()
	_, refresh := m.Update(msg)
	if m.backend != next || m.investigator != a || refresh == nil || m.switching {
		t.Fatal("switch failed")
	}
	assertClosed(t, old)
	m.Close()
	m.Close()
	assertClosed(t, a)
}
func TestContextCancelFailureAndAbandonedSuccessCleanup(t *testing.T) {
	m, b, _ := testModel(t)
	m.ConfigureContexts([]ContextChoice{{Name: "next"}}, func(context.Context, string) (Session, error) { return Session{}, errors.New("secret error") })
	m.showContexts()
	m.contextKey("esc")
	if m.contextMenu {
		t.Fatal("menu did not cancel")
	}
	m.showContexts()
	cmd := m.contextKey("enter")
	m.Update(cmd())
	if m.backend != b || m.switching {
		t.Fatal("failed switch lost previous backend")
	}
	a := closingAgent(newFakeBackend())
	release := make(chan struct{})
	started := make(chan struct{})
	result := make(chan tea.Msg, 1)
	m.ConfigureContexts([]ContextChoice{{Name: "next"}}, func(context.Context, string) (Session, error) {
		close(started)
		<-release
		return Session{a.backend, a}, nil
	})
	m.showContexts()
	cmd = m.contextKey("enter")
	go func() { result <- cmd() }()
	<-started
	m.contextKey("esc")
	close(release)
	msg := <-result
	assertClosed(t, a)
	m.Update(msg)
	if m.backend != b || m.switching {
		t.Fatal("late canceled result activated")
	}
	// A completed result that is never delivered must still release on quit.
	a2 := closingAgent(newFakeBackend())
	m.ConfigureContexts([]ContextChoice{{Name: "next"}}, func(context.Context, string) (Session, error) { return Session{a2.backend, a2}, nil })
	m.showContexts()
	cmd = m.contextKey("enter")
	_ = cmd()
	m.Close()
	assertClosed(t, a2)
}

type pageBackend struct {
	*fakeBackend
	cursors []string
}

func (b *pageBackend) ListResourcesPage(_ context.Context, _, _, _ string, limit int, cursor string) core.ResourcePage {
	if limit != 50 {
		panic("unbounded")
	}
	b.cursors = append(b.cursors, cursor)
	if cursor == "" {
		return core.ResourcePage{Result: core.ToolResult{Resources: []core.Resource{fakePod("first")}, Data: map[string]any{"limited": true}}, NextCursor: "page-two"}
	}
	return core.ResourcePage{Result: core.ToolResult{Resources: []core.Resource{fakePod("second")}}}
}
func TestBoundedPageNavigationReplacesRowsAndClearsSelection(t *testing.T) {
	b := &pageBackend{fakeBackend: newFakeBackend()}
	m := New(context.Background(), b, &fakeInvestigator{backend: b.fakeBackend}, false)
	defer m.Close()
	runCommand(t, m, m.refresh())
	if m.nextCursor != "page-two" || len(m.resources) != 1 {
		t.Fatal("first page")
	}
	firstGen := m.listGeneration
	cmd := m.nextPage()
	if m.selected != nil || len(m.resources) != 0 {
		t.Fatal("old selection retained")
	}
	if m.nextPage() != nil {
		t.Fatal("concurrent paging")
	}
	runCommand(t, m, cmd)
	if len(m.resources) != 1 || m.resources[0].Ref.Name != "second" || m.nextCursor != "" {
		t.Fatal("page accumulation")
	}
	m.Update(listResultMsg{generation: firstGen, resources: []core.Resource{fakePod("stale")}})
	if m.resources[0].Ref.Name != "second" {
		t.Fatal("old page accepted")
	}
	runCommand(t, m, m.previousPage())
	if m.resources[0].Ref.Name != "first" || len(m.pageHistory) != 0 {
		t.Fatal("previous page")
	}
	m.pageHistory = make([]string, 100)
	if m.nextPage() != nil {
		t.Fatal("unbounded cursor history")
	}
	runCommand(t, m, m.refresh())
	if len(m.pageHistory) != 0 || m.pageCursor != "" {
		t.Fatal("refresh did not reset")
	}
}

func TestContextMenuViewportAndScope(t *testing.T) {
	m, _, _ := testModel(t)
	choices := make([]ContextChoice, 25)
	for i := range choices {
		choices[i] = ContextChoice{Name: fmt.Sprintf("context-%02d-", i) + strings.Repeat("x", 160), Namespace: ""}
	}
	m.ConfigureContexts(choices, func(context.Context, string) (Session, error) { return Session{}, nil })
	m.showContexts()
	m.contextChoice = 23
	for _, size := range [][2]int{{65, 22}, {140, 44}} {
		m.resize(size[0], size[1])
		view := ansi.Strip(m.View())
		lines := strings.Split(view, "\n")
		if len(lines) != size[1] {
			t.Fatalf("height %d", len(lines))
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > size[0] {
				t.Fatal("overflow")
			}
		}
		if !strings.Contains(view, "> context-23") || !strings.Contains(view, "namespace: all namespaces") {
			t.Fatal("selection or scope missing")
		}
	}
	if path := os.Getenv("KUBIX_CONTEXT_SNAPSHOT_PATH"); path != "" {
		if err := os.WriteFile(path, []byte(m.View()), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRepeatedContextSwitchCleanup(t *testing.T) {
	b := newFakeBackend()
	initial := closingAgent(b)
	m := New(context.Background(), b, initial, false)
	defer m.Close()
	made := []*closingInvestigator{}
	m.ConfigureContexts([]ContextChoice{{Name: "one"}, {Name: "two"}}, func(context.Context, string) (Session, error) {
		a := closingAgent(newFakeBackend())
		made = append(made, a)
		return Session{a.backend, a}, nil
	})
	for i := 0; i < 4; i++ {
		m.showContexts()
		m.contextChoice = i % 2
		cmd := m.contextKey("enter")
		m.Update(cmd())
	}
	assertClosed(t, initial)
	for _, a := range made[:3] {
		assertClosed(t, a)
	}
	m.Close()
	assertClosed(t, made[3])
}
