package tui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/x/ansi"
)

// The renderer and mouse handling share these screen coordinates.
type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool {
	return x >= r.x && y >= r.y && x < r.x+r.w && y < r.y+r.h
}

type workspaceLayout struct {
	roomy                                                         bool
	left, resources, inspector                                    rect
	conversation, prompt, resourceRows, kindSelector, kindOptions rect
	buttons                                                       [3]rect
	container, detail                                             rect
	toolbarHeight, selectionHeight, buttonHeight                  int
}

func (m *Model) resize(width, height int) {
	m.width, m.height = max(1, width), max(1, height)
	l := workspaceLayout{roomy: width >= 110 && height >= 32, toolbarHeight: 1, selectionHeight: 1, buttonHeight: 1}
	m.bodyY = 2
	bottomGap := 0
	if l.roomy {
		m.bodyY = 4 // Three header rows and one workspace gutter.
		bottomGap = 1
		l.toolbarHeight, l.selectionHeight, l.buttonHeight = 3, 2, 3
	}
	if m.demo {
		m.bodyY++
	}
	m.bodyHeight = max(8, m.height-m.bodyY-1-bottomGap)
	m.leftWidth = max(20, (m.width-3)*2/5)
	m.rightWidth = max(20, m.width-m.leftWidth-3)
	m.resourceHeight = max(7, (m.bodyHeight-1)*2/5)
	m.inspectorHeight = max(7, m.bodyHeight-m.resourceHeight-1)
	l.left = rect{1, m.bodyY, m.leftWidth, m.bodyHeight}
	l.resources = rect{m.leftWidth + 2, m.bodyY, m.rightWidth, m.resourceHeight}
	l.inspector = rect{l.resources.x, m.bodyY + m.resourceHeight + 1, m.rightWidth, m.inspectorHeight}
	l.conversation = rect{l.left.x + 2, m.bodyY + 3, m.leftWidth - 6, max(1, m.bodyHeight-10)}
	l.prompt = rect{l.left.x + 2, m.bodyY + m.bodyHeight - 5, m.leftWidth - 4, 3}
	m.prompt.Width = max(1, l.prompt.w-5) // Leading space, prompt glyph, and the box edges.
	m.conversation.Width, m.conversation.Height = max(1, l.conversation.w), l.conversation.h

	tableWidth := max(1, m.rightWidth-2)
	rowCount := max(1, m.resourceHeight-l.toolbarHeight-4)
	l.resourceRows = rect{l.resources.x + 1, m.bodyY + l.toolbarHeight + 2, tableWidth, rowCount}
	selectorWidth := min(23, max(14, tableWidth/2))
	l.kindSelector = rect{l.resources.x + m.rightWidth - 2 - selectorWidth, m.bodyY + 1 + l.toolbarHeight/2, selectorWidth, 1}
	l.kindOptions = rect{l.kindSelector.x, l.kindSelector.y + 1, selectorWidth, len(resourceKinds) + 2}
	m.table.SetWidth(tableWidth)
	kindWidth := min(11, max(4, tableWidth/9))
	namespaceWidth := min(12, max(6, tableWidth/8))
	statusWidth := min(18, max(10, tableWidth/4))
	nameWidth := max(1, tableWidth-kindWidth-namespaceWidth-statusWidth-8)
	m.table.SetColumns([]table.Column{{Title: "KIND", Width: kindWidth}, {Title: "NAMESPACE", Width: namespaceWidth}, {Title: "NAME", Width: nameWidth}, {Title: "STATUS", Width: statusWidth}})
	m.table.SetHeight(rowCount + 1) // Bubble table includes its one-row header.
	m.syncTable()

	buttonY := l.inspector.y + 2 + l.selectionHeight
	x := l.inspector.x + 2
	for i, w := range []int{10, 8, 12} {
		l.buttons[i] = rect{x, buttonY, w, l.buttonHeight}
		x += w + 1
	}
	l.container = rect{l.inspector.x + 2, buttonY + l.buttonHeight, m.rightWidth - 4, 1}
	l.detail = rect{l.inspector.x + 2, l.container.y + 1, m.rightWidth - 6, max(1, l.inspector.y+l.inspector.h-1-(l.container.y+1))}
	m.inspector.Width, m.inspector.Height = max(1, l.detail.w), l.detail.h
	m.layout = l
	if m.evidenceMenu {
		m.moveEvidence(0)
	}
	if renderer, err := newMarkdownRenderer(m.conversation.Width); err == nil {
		m.markdown = renderer
	}
	m.renderConversation(false)
	m.inspector.SetContent(ansi.Hardwrap(m.detailText, m.inspector.Width, true))
}
