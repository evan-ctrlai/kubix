package tui

import (
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// newMarkdownRenderer keeps conversation colors consistent with the workspace.
// The viewport supplies its own padding, so Markdown adds no outer margin.
func newMarkdownRenderer(width int) (*glamour.TermRenderer, error) {
	style := styles.DarkStyleConfig
	body, heading, amber := "#d8e3f0", "#67e8f9", "#e3b66c"
	muted, codeBackground, blockBackground := "#95acc3", "#192432", "#0e1a29"
	green := "#b6ed78"
	zero, one := uint(0), uint(1)
	bold, underline := true, true

	style.Document = ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{Color: &body},
		Margin:         &zero,
	}
	style.Paragraph = ansi.StyleBlock{Margin: &zero}
	style.Heading = ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{Color: &heading, Bold: &bold, BlockSuffix: "\n"},
	}
	// Clear the standard theme's hash prefixes and colored H1 badge.
	style.H1, style.H2, style.H3 = ansi.StyleBlock{}, ansi.StyleBlock{}, ansi.StyleBlock{}
	style.H4, style.H5, style.H6 = ansi.StyleBlock{}, ansi.StyleBlock{}, ansi.StyleBlock{}
	style.Strong = ansi.StylePrimitive{Color: &body, Bold: &bold}
	style.Code = ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Color: &amber, BackgroundColor: &codeBackground, Prefix: " ", Suffix: " ",
		},
	}
	style.CodeBlock = ansi.StyleCodeBlock{
		StyleBlock: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: &body, BackgroundColor: &blockBackground},
			Margin:         &zero,
		},
		Chroma: &ansi.Chroma{
			Text:           ansi.StylePrimitive{Color: &body},
			Error:          ansi.StylePrimitive{Color: &amber},
			Comment:        ansi.StylePrimitive{Color: &muted},
			CommentPreproc: ansi.StylePrimitive{Color: &muted},
			Keyword:        ansi.StylePrimitive{Color: &heading},
			Operator:       ansi.StylePrimitive{Color: &body},
			Punctuation:    ansi.StylePrimitive{Color: &muted},
			Name:           ansi.StylePrimitive{Color: &body},
			NameFunction:   ansi.StylePrimitive{Color: &heading},
			LiteralString:  ansi.StylePrimitive{Color: &amber},
			LiteralNumber:  ansi.StylePrimitive{Color: &green},
			Background:     ansi.StylePrimitive{BackgroundColor: &blockBackground},
		},
	}
	quoteToken := "│ "
	style.BlockQuote = ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{Color: &muted},
		Indent:         &one, IndentToken: &quoteToken,
	}
	style.Link = ansi.StylePrimitive{Color: &heading, Underline: &underline}
	style.LinkText = ansi.StylePrimitive{Color: &heading}
	style.HorizontalRule.Color = &muted
	style.Table.StylePrimitive.Color = &body
	style.List.LevelIndent = 2
	return glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(max(8, width)),
		glamour.WithColorProfile(lipgloss.ColorProfile()),
	)
}
