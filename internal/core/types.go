// Package core contains the contracts shared by the terminal, investigator and backends.
package core

import "context"

var Kinds = []string{"pods", "replicasets", "deployments", "services", "endpointslices", "events", "nodes"}

func ValidKind(kind string) bool {
	for _, known := range Kinds {
		if kind == known {
			return true
		}
	}
	return false
}

type ResourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

func (r ResourceRef) Key() string {
	return r.Kind + ":" + r.Namespace + ":" + r.Name + ":" + r.UID
}

func (r ResourceRef) DisplayName() string {
	name := r.Kind + "/" + r.Name
	if r.Namespace != "" {
		name += " in " + r.Namespace
	}
	return name
}

type Resource struct {
	Ref     ResourceRef    `json:"ref"`
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details"`
}

type ToolResult struct {
	Tool      string         `json:"tool"`
	Resources []Resource     `json:"resources"`
	Data      map[string]any `json:"data"`
	Errors    []string       `json:"errors"`
}

func ErrorResult(tool, message string) ToolResult {
	return ToolResult{Tool: tool, Errors: []string{message}}
}

type Answer struct {
	Text          string         `json:"text"`
	Resources     []Resource     `json:"resources"`
	Evidence      []ToolResult   `json:"evidence"`
	Investigation *Investigation `json:"investigation,omitempty"`
}

// Backend deliberately exposes only reads. Empty namespace uses session scope.
type Backend interface {
	ContextName() string
	Namespace() string
	ListResources(ctx context.Context, kind, namespace, query string, limit int) ToolResult
	GetResource(ctx context.Context, kind, namespace, name string) ToolResult
	PodLogs(ctx context.Context, namespace, pod, container string, previous bool) ToolResult
}

type Investigator interface {
	Ask(ctx context.Context, question string, selected *ResourceRef, onEvent func(string)) Answer
	ModeLabel() string
}
