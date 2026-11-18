// Package kube exposes bounded, read-only Kubernetes operations.
package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // Honor kubeconfig authentication plugins.
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"kubix/internal/core"
	"kubix/internal/redact"
)

const (
	readTimeout = 10 * time.Second
	maxList     = 100
	logBytes    = 16000
)

// Backend holds a client and its fixed context/namespace scope. An empty
// configured namespace permits all namespaces; an empty method argument inherits
// the configured scope. No kubeconfig or cluster mutation is implemented here.
type Backend struct {
	client      kubernetes.Interface
	contextName string
	namespace   string
}

type discardPersister struct{}

func (discardPersister) Persist(map[string]string) error { return nil }

// New loads kubeconfig into an instance-owned REST configuration. Missing default
// configuration may use in-cluster credentials. An explicit context or config
// path never silently falls back to a different cluster.
func New(contextName, namespace, kubeconfig string) (*Backend, error) {
	if namespace != "" && len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("invalid Kubernetes namespace")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, errors.New("unable to load kubeconfig; check the selected file and context")
	}
	var cfg *rest.Config
	selected := contextName
	if selected == "" {
		selected = raw.CurrentContext
	}
	if contextName == "" && kubeconfig == "" && len(raw.Contexts) == 0 && len(raw.Clusters) == 0 && len(raw.AuthInfos) == 0 {
		cfg, err = rest.InClusterConfig()
		selected = "in-cluster"
	} else {
		// A nil ConfigAccess avoids a writer; AuthConfigPersister also explicitly
		// discards legacy auth-provider refresh persistence below.
		cfg, err = clientcmd.NewNonInteractiveClientConfig(*raw, selected, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	}
	if err != nil {
		return nil, errors.New("unable to load Kubernetes credentials; check kubeconfig/context or in-cluster credentials")
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = readTimeout
	cfg.AuthConfigPersister = discardPersister{}
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.New("unable to initialize the Kubernetes client")
	}
	return NewWithClient(api, selected, namespace), nil
}

// NewWithClient injects a Kubernetes client for tests or an existing application.
func NewWithClient(api kubernetes.Interface, contextName, namespace string) *Backend {
	if contextName == "" {
		contextName = "injected"
	}
	return &Backend{client: api, contextName: contextName, namespace: namespace}
}

func (b *Backend) ContextName() string { return b.contextName }
func (b *Backend) Namespace() string   { return b.namespace }

func (b *Backend) scope(kind, namespace string) (string, error) {
	switch kind {
	case "pods", "deployments", "replicasets", "services", "endpointslices", "events", "nodes":
	default:
		return "", errors.New("allowed kinds: pods, deployments, replicasets, services, events, nodes")
	}
	if namespace == "" {
		namespace = b.namespace
	}
	if namespace != "" && len(validation.IsDNS1123Label(namespace)) != 0 {
		return "", errors.New("invalid namespace")
	}
	if b.namespace != "" && namespace != b.namespace {
		return "", fmt.Errorf("namespace is outside the configured scope (%s)", b.namespace)
	}
	if kind == "nodes" {
		return "", nil
	}
	return namespace, nil
}

func failure(tool string, err error) core.ToolResult {
	message := "Kubernetes API read failed or timed out; check cluster connectivity."
	switch {
	case apierrors.IsUnauthorized(err):
		message = "Kubernetes authentication failed (401); check the selected context credentials."
	case apierrors.IsForbidden(err):
		message = "Kubernetes denied this read (403); check read-only RBAC for the resource."
	case apierrors.IsNotFound(err):
		message = "The Kubernetes resource or requested logs no longer exist (404)."
	case apierrors.IsTooManyRequests(err):
		message = "Kubernetes rate limited the read (429); try again shortly."
	case apierrors.IsBadRequest(err):
		message = "Kubernetes could not provide the requested data; previous container logs may not exist."
	case errors.Is(err, context.Canceled):
		message = "Kubernetes read was canceled."
	}
	return invalid(tool, message)
}

func invalid(tool, message string) core.ToolResult {
	return core.ToolResult{Tool: tool, Errors: []string{redact.Text(message, 500)}}
}

// ListResources returns one API page. Query filtering is local to that bounded
// page, and its metadata makes incomplete searches explicit.
func (b *Backend) ListResources(ctx context.Context, kind, namespace, query string, limit int) core.ToolResult {
	const tool = "list_resources"
	scope, err := b.scope(kind, namespace)
	if err != nil {
		return invalid(tool, err.Error())
	}
	if limit < 1 || limit > maxList {
		return invalid(tool, "limit must be between 1 and 100")
	}
	if len([]rune(query)) > 200 {
		return invalid(tool, "query must be at most 200 characters")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	opts := metav1.ListOptions{Limit: int64(limit)}
	resources := []core.Resource{}
	continuation := ""
	fetched := 0
	switch kind {
	case "pods":
		list, readErr := b.client.CoreV1().Pods(scope).List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, podResource(&list.Items[i]))
		}
	case "deployments":
		list, readErr := b.client.AppsV1().Deployments(scope).List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, deploymentResource(&list.Items[i]))
		}
	case "replicasets":
		list, readErr := b.client.AppsV1().ReplicaSets(scope).List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, replicaSetResource(&list.Items[i]))
		}
	case "services":
		list, readErr := b.client.CoreV1().Services(scope).List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, serviceResource(&list.Items[i]))
		}
	case "endpointslices":
		list, readErr := b.client.DiscoveryV1().EndpointSlices(scope).List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, endpointSliceResource(&list.Items[i]))
		}
	case "events":
		list, readErr := b.client.CoreV1().Events(scope).List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, eventResource(&list.Items[i]))
		}
	case "nodes":
		list, readErr := b.client.CoreV1().Nodes().List(ctx, opts)
		if readErr != nil {
			return failure(tool, readErr)
		}
		continuation, fetched = list.Continue, len(list.Items)
		for i := 0; i < min(len(list.Items), limit); i++ {
			resources = append(resources, nodeResource(&list.Items[i]))
		}
	}
	filtered := []core.Resource{}
	for _, resource := range resources {
		if !validRef(resource.Ref) || (scope != "" && resource.Ref.Namespace != scope) {
			continue
		}
		haystack := strings.ToLower(resource.Ref.Namespace + " " + resource.Ref.Name + " " + resource.Status + " " + resource.Summary)
		if strings.Contains(haystack, strings.ToLower(query)) {
			filtered = append(filtered, resource)
		}
	}
	return core.ToolResult{Tool: tool, Resources: filtered, Data: map[string]any{
		"kind": kind, "namespace": scope, "query": query, "limit": limit,
		"fetched": min(fetched, limit), "returned": len(filtered),
		"limited":     continuation != "" || fetched >= limit,
		"truncated":   continuation != "" || fetched > limit,
		"query_scope": "Substring filter over one bounded API page; no pagination is followed",
	}}
}

func validRef(ref core.ResourceRef) bool {
	return len(validation.IsDNS1123Subdomain(ref.Name)) == 0 &&
		(ref.Kind == "nodes" || (ref.Namespace != "" && len(validation.IsDNS1123Label(ref.Namespace)) == 0))
}

func (b *Backend) GetResource(ctx context.Context, kind, namespace, name string) core.ToolResult {
	const tool = "get_resource"
	scope, err := b.scope(kind, namespace)
	if err != nil {
		return invalid(tool, err.Error())
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return invalid(tool, "invalid Kubernetes resource name")
	}
	if kind != "nodes" && scope == "" {
		return invalid(tool, "a namespace is required to inspect a specific resource")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var resource core.Resource
	switch kind {
	case "pods":
		obj, readErr := b.client.CoreV1().Pods(scope).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = podResource(obj)
	case "deployments":
		obj, readErr := b.client.AppsV1().Deployments(scope).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = deploymentResource(obj)
	case "replicasets":
		obj, readErr := b.client.AppsV1().ReplicaSets(scope).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = replicaSetResource(obj)
	case "services":
		obj, readErr := b.client.CoreV1().Services(scope).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = serviceResource(obj)
	case "endpointslices":
		obj, readErr := b.client.DiscoveryV1().EndpointSlices(scope).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = endpointSliceResource(obj)
	case "events":
		obj, readErr := b.client.CoreV1().Events(scope).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = eventResource(obj)
	case "nodes":
		obj, readErr := b.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return failure(tool, readErr)
		}
		resource = nodeResource(obj)
	}
	if !validRef(resource.Ref) || resource.Ref.Name != name || resource.Ref.Namespace != scope {
		return invalid(tool, "Kubernetes returned a resource with a different identity")
	}
	return core.ToolResult{Tool: tool, Resources: []core.Resource{resource}}
}

// PodLogs validates container membership, requests a bounded sample, and rechecks
// the UID because the Kubernetes log endpoint has no UID precondition.
func (b *Backend) PodLogs(ctx context.Context, namespace, pod, container string, previous bool) core.ToolResult {
	const tool = "get_pod_logs"
	scope, err := b.scope("pods", namespace)
	if err != nil {
		return invalid(tool, err.Error())
	}
	if scope == "" {
		return invalid(tool, "a namespace is required to read pod logs")
	}
	if len(validation.IsDNS1123Subdomain(pod)) != 0 {
		return invalid(tool, "invalid Kubernetes pod name")
	}
	if container != "" && len(validation.IsDNS1123Label(container)) != 0 {
		return invalid(tool, "invalid container name")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	obj, err := b.client.CoreV1().Pods(scope).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return failure(tool, err)
	}
	resource := podResource(obj)
	if obj.Name != pod || obj.Namespace != scope || obj.UID == "" {
		return invalid(tool, "pod identity could not be verified; logs were not read")
	}
	names := []string{}
	for _, item := range obj.Spec.Containers {
		names = append(names, item.Name)
	}
	for _, item := range obj.Spec.InitContainers {
		names = append(names, item.Name)
	}
	if container == "" && len(names) == 1 {
		container = names[0]
	}
	found := false
	for _, name := range names {
		if name == container {
			found = true
		}
	}
	if !found {
		return invalid(tool, "choose a container from this pod: "+strings.Join(names[:min(len(names), 20)], ", "))
	}
	tail, bytesLimit, since := int64(100), int64(logBytes), int64(900)
	stream, err := b.client.CoreV1().Pods(scope).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: previous, Follow: false, Timestamps: true,
		TailLines: &tail, LimitBytes: &bytesLimit, SinceSeconds: &since,
	}).Stream(ctx)
	if err != nil {
		return failure(tool, err)
	}
	logs, readErr := io.ReadAll(io.LimitReader(stream, logBytes+1))
	_ = stream.Close()
	if readErr != nil {
		return failure(tool, readErr)
	}
	final, err := b.client.CoreV1().Pods(scope).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return failure(tool, err)
	}
	if final.UID != obj.UID || final.Name != obj.Name || final.Namespace != obj.Namespace {
		return invalid(tool, "pod was replaced during collection; ambiguous log samples were discarded")
	}
	truncated := len(logs) > logBytes
	if truncated {
		logs = logs[:logBytes]
	}
	return core.ToolResult{Tool: tool, Resources: []core.Resource{resource}, Data: map[string]any{
		"logs": redact.Text(string(logs), logBytes), "container": container,
		"previous": previous, "pod_uid": string(obj.UID), "tail_lines": 100,
		"limit_bytes": logBytes, "since_seconds": 900, "bounded_sample": true,
		"truncated": truncated,
	}}
}
