package kube

import (
	"errors"
	"sort"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ContextChoice contains only kubeconfig context metadata, never credentials or
// server URLs. Namespace is the effective default for this context.
type ContextChoice struct {
	Name      string
	Namespace string
	Current   bool
}

// ScopeOptions distinguishes an explicit namespace from the context default.
// Only AllNamespaces permits an empty namespace (cluster-wide namespaced reads).
type ScopeOptions struct {
	Namespace         string
	NamespaceExplicit bool
	AllNamespaces     bool
}

func loadConfig(path string) (*clientcmdapi.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, errors.New("unable to load kubeconfig; check the selected file and context")
	}
	return raw, nil
}

// ListContexts reads metadata only. It does not resolve credentials, execute
// authentication plugins, contact a cluster, or modify current-context.
func ListContexts(kubeconfig string) ([]ContextChoice, error) {
	raw, err := loadConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	choices := make([]ContextChoice, 0, len(raw.Contexts))
	for name, c := range raw.Contexts {
		if c == nil {
			continue
		}
		ns := c.Namespace
		if ns == "" {
			ns = "default"
		}
		choices = append(choices, ContextChoice{Name: name, Namespace: ns, Current: name == raw.CurrentContext})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Name < choices[j].Name })
	return choices, nil
}

// NewScoped constructs an independent session without writing kubeconfig.
// Omitted namespace follows the selected context, not the process's old session.
// New is retained for callers that already supply a resolved namespace.
func NewScoped(contextName, kubeconfig string, scope ScopeOptions) (*Backend, error) {
	if scope.AllNamespaces && scope.NamespaceExplicit {
		return nil, errors.New("use namespace or all namespaces, not both")
	}
	if scope.NamespaceExplicit && (scope.Namespace == "" || len(validation.IsDNS1123Label(scope.Namespace)) != 0) {
		return nil, errors.New("invalid Kubernetes namespace")
	}
	raw, err := loadConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	selected := contextName
	if selected == "" {
		selected = raw.CurrentContext
	}
	namespace := "default"
	if c := raw.Contexts[selected]; c != nil && c.Namespace != "" {
		namespace = c.Namespace
	}
	if scope.NamespaceExplicit {
		namespace = scope.Namespace
	}
	if scope.AllNamespaces {
		namespace = ""
	}
	if namespace != "" && len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("invalid Kubernetes namespace")
	}
	return newFromConfig(contextName, namespace, kubeconfig, raw)
}
