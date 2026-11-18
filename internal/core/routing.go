package core

import "context"

// LabelSelectorReader is an optional read-only capability. Implementations must
// apply equality selectors on the server and return bounded-page metadata.
type LabelSelectorReader interface {
	ListByLabels(ctx context.Context, kind, namespace string, selector map[string]string, limit int) ToolResult
}
