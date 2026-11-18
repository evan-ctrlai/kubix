package core

import "context"

const MaxResourceCursorBytes = 8192

// ResourcePage is one bounded read. NextCursor is opaque and must be reused
// only with the same kind, namespace, query and page size. It is not evidence
// or a model tool argument. An empty NextCursor means no further page is known.
type ResourcePage struct {
	Result     ToolResult
	NextCursor string
}

// ResourcePageReader is an optional capability for explicit UI pagination.
// Each invocation performs at most one API list operation, never an auto-scan.
type ResourcePageReader interface {
	ListResourcesPage(ctx context.Context, kind, namespace, query string, limit int, cursor string) ResourcePage
}
