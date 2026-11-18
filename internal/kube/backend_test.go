package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"kubix/internal/core"
)

var _ core.Backend = (*Backend)(nil)

func testPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID("uid-" + name),
			Annotations: map[string]string{"secret": "annotation-must-not-leak"}},
		Spec: corev1.PodSpec{NodeName: "node-1", RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{Name: "app", Image: "checkout:v2",
				Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "env-must-not-leak"}}, Args: []string{"--password=arg-must-not-leak"}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 8,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "password=state-secret backing off"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}}},
	}
}

func TestScopeAndInvalidArgumentsDoNotCallAPI(t *testing.T) {
	api := fake.NewClientset(testPod("checkout", "apps"))
	b := NewWithClient(api, "test", "apps")
	ctx := context.Background()
	results := []core.ToolResult{
		b.ListResources(ctx, "secrets", "", "", 50),
		b.ListResources(ctx, "pods", "other", "", 50),
		b.ListResources(ctx, "nodes", "other", "", 50),
		b.ListResources(ctx, "pods", "", "", 101),
		b.ListResources(ctx, "pods", "", strings.Repeat("x", 201), 50),
		b.GetResource(ctx, "pods", "other", "checkout"),
		b.GetResource(ctx, "pods", "apps", "../../secrets"),
		b.PodLogs(ctx, "other", "checkout", "", false),
		b.PodLogs(ctx, "apps", "checkout", "../../secret", false),
	}
	for _, result := range results {
		if len(result.Errors) == 0 {
			t.Fatalf("missing error: %#v", result)
		}
	}
	if len(api.Actions()) != 0 {
		t.Fatalf("invalid inputs reached API: %v", api.Actions())
	}
	if _, err := New("", "*", ""); err == nil {
		t.Fatal("invalid namespace accepted")
	}
}

func TestPodListHasActualIdentityWhitelistedFieldsAndCrashStatus(t *testing.T) {
	api := fake.NewClientset(testPod("checkout", "apps"))
	b := NewWithClient(api, "test", "apps")
	result := b.ListResources(context.Background(), "pods", "", "", 50)
	if len(result.Errors) > 0 || len(result.Resources) != 1 {
		t.Fatalf("unexpected result %#v", result)
	}
	resource := result.Resources[0]
	if resource.Ref.UID != "uid-checkout" || resource.Ref.Namespace != "apps" || resource.Status != "CrashLoopBackOff" {
		t.Fatalf("bad resource identity/status: %#v", resource)
	}
	if !strings.Contains(resource.Summary, "OOMKilled") {
		t.Fatalf("missing termination cause: %s", resource.Summary)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"annotation-must-not-leak", "env-must-not-leak", "arg-must-not-leak", "state-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("leaked %s", secret)
		}
	}
	for _, action := range api.Actions() {
		if action.GetVerb() != "list" && action.GetVerb() != "get" {
			t.Errorf("mutating action: %v", action)
		}
	}
}

func TestPodResourceExposesBoundedRedactedLabelsForServiceSelectors(t *testing.T) {
	pod := testPod("checkout", "apps")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	pod.Labels = map[string]string{"app.kubernetes.io/name": "checkout", "tier": "backend", "password": "password=label-secret"}
	resource := NewWithClient(fake.NewClientset(pod), "test", "apps").GetResource(context.Background(), "pods", "apps", pod.Name).Resources[0]
	labels, ok := resource.Details["labels"].(map[string]any)
	if !ok || labels["app.kubernetes.io/name"] != "checkout" || labels["tier"] != "backend" {
		t.Fatalf("Pod labels were not projected for selector comparison: %#v", resource.Details)
	}
	encoded, _ := json.Marshal(resource)
	if !strings.Contains(string(encoded), `"container_port":8080`) {
		t.Fatal("declared container port was not exposed for target-port comparison")
	}
	if strings.Contains(string(encoded), "label-secret") {
		t.Fatal("sensitive label value was not redacted")
	}

	for n := range 55 {
		pod.Labels[fmt.Sprintf("label-%02d", n)] = "value"
	}
	resource = NewWithClient(fake.NewClientset(pod), "test", "apps").GetResource(context.Background(), "pods", "apps", pod.Name).Resources[0]
	labels, ok = resource.Details["labels"].(map[string]any)
	if !ok || len(labels) != 50 || resource.Details["labels_truncated"] != true {
		t.Fatalf("large label sets were not bounded and marked incomplete: count=%d details=%#v", len(labels), resource.Details)
	}
}

func TestEndpointSlicesExposeServiceAndPodTargetsAndPreserveForbiddenGap(t *testing.T) {
	port := int32(8080)
	ready, serving := true, true
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "checkout-abc", Namespace: "apps", UID: "uid-slice",
		Labels: map[string]string{discoveryv1.LabelServiceName: "checkout"}}, AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{{Name: ptr("http"), Port: &port}}, Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{"10.0.0.9"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready, Serving: &serving},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: "apps", Name: "checkout-abc", UID: "uid-checkout-abc"},
		}}}
	api := fake.NewClientset(slice)
	b := NewWithClient(api, "test", "apps")
	listed := b.ListResources(context.Background(), "endpointslices", "", "checkout", 50)
	if len(listed.Errors) > 0 || len(listed.Resources) != 1 || listed.Resources[0].Summary == "" {
		t.Fatalf("EndpointSlice list failed: %#v", listed)
	}
	got := b.GetResource(context.Background(), "endpointslices", "apps", slice.Name)
	if len(got.Errors) > 0 || len(got.Resources) != 1 {
		t.Fatalf("EndpointSlice get failed: %#v", got)
	}
	resource := got.Resources[0]
	if resource.Ref.UID != "uid-slice" || resource.Details["service_name"] != "checkout" {
		t.Fatalf("EndpointSlice lost its Service identity: %#v", resource)
	}
	endpoints, ok := resource.Details["endpoints"].([]any)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("EndpointSlice target evidence missing: %#v", resource.Details)
	}
	encoded, _ := json.Marshal(resource.Details)
	for _, expected := range []string{"checkout-abc", "uid-checkout-abc", "ready", "8080"} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("EndpointSlice details omit %q: %s", expected, encoded)
		}
	}
	if strings.Contains(string(encoded), "10.0.0.9") {
		t.Fatal("raw endpoint addresses should not be exposed")
	}
	for _, action := range api.Actions() {
		if action.GetVerb() != "list" && action.GetVerb() != "get" {
			t.Fatalf("EndpointSlice read used a mutating action: %#v", action)
		}
	}

	denied := fake.NewClientset()
	denied.PrependReactor("list", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, "checkout", errors.New("secret authorization detail"))
	})
	result := NewWithClient(denied, "test", "apps").ListResources(context.Background(), "endpointslices", "", "", 50)
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "403") || !strings.Contains(result.Errors[0], "denied") || strings.Contains(result.Errors[0], "secret") || len(result.Resources) != 0 {
		t.Fatalf("permission failure was hidden or treated as empty endpoints: %#v", result)
	}
}

func ptr[T any](value T) *T { return &value }

func TestAllNamespaceModePreservesIdentityAndRequiresNamespaceForGet(t *testing.T) {
	api := fake.NewClientset(testPod("checkout", "apps"), testPod("payments", "payments"))
	b := NewWithClient(api, "test", "")
	result := b.ListResources(context.Background(), "pods", "", "", 50)
	if len(result.Resources) != 2 {
		t.Fatalf("unexpected list: %#v", result)
	}
	if len(b.GetResource(context.Background(), "pods", "", "checkout").Errors) == 0 {
		t.Fatal("unscoped get allowed")
	}
	if result := b.GetResource(context.Background(), "pods", "apps", "checkout"); len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
}

func TestBoundedQueryDoesNotFollowPagination(t *testing.T) {
	api := fake.NewClientset()
	api.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next-page"},
			Items: []corev1.Pod{*testPod("checkout", "apps"), *testPod("other", "apps")}}, nil
	})
	result := NewWithClient(api, "test", "apps").ListResources(context.Background(), "pods", "", "CHECKOUT", 2)
	if len(result.Resources) != 1 || result.Resources[0].Ref.Name != "checkout" {
		t.Fatalf("query result: %#v", result)
	}
	if result.Data["truncated"] != true || result.Data["limited"] != true {
		t.Fatalf("missing pagination bounds: %#v", result.Data)
	}
	if len(api.Actions()) != 1 {
		t.Fatal("unexpected pagination calls")
	}
}

func TestNodeClusterScopeAndOtherResourceKinds(t *testing.T) {
	replicas := int32(3)
	api := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: "node-uid"}, Spec: corev1.NodeSpec{Unschedulable: true},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "apps", Generation: 2},
			Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 2}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "apps"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.12", Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}}}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "checkout.abc", Namespace: "apps"}, Type: "Warning", Reason: "BackOff", Message: "password=event-secret backing off"},
	)
	b := NewWithClient(api, "test", "apps")
	ctx := context.Background()
	nodes := b.ListResources(ctx, "nodes", "", "", 50)
	if len(nodes.Resources) != 1 || nodes.Resources[0].Ref.Namespace != "" || nodes.Resources[0].Status != "Ready" {
		t.Fatalf("nodes: %#v", nodes)
	}
	if !strings.Contains(nodes.Resources[0].Summary, "scheduling disabled") {
		t.Fatal("missing node scheduling state")
	}
	for _, tc := range []struct{ kind, name, status string }{{"deployments", "checkout", "Progressing"}, {"services", "checkout", "ClusterIP"}, {"events", "checkout.abc", "Warning"}} {
		result := b.GetResource(ctx, tc.kind, "apps", tc.name)
		if len(result.Errors) > 0 || len(result.Resources) != 1 || result.Resources[0].Status != tc.status {
			t.Fatalf("%s: %#v", tc.kind, result)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "event-secret") {
			t.Fatal("event message was not redacted")
		}
	}
}

func TestEventTimeIsRetainedWhenLegacyTimestampsAreAbsent(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout.abc", Namespace: "apps"},
		EventTime:  metav1.NewMicroTime(time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)),
		Type:       "Warning", Reason: "BackOff",
	}
	result := NewWithClient(fake.NewClientset(event), "test", "apps").GetResource(context.Background(), "events", "", event.Name)
	if len(result.Errors) > 0 || len(result.Resources) != 1 || result.Resources[0].Details["event_time"] != "2026-11-13T15:00:00Z" {
		t.Fatalf("event occurrence time was omitted: %#v", result)
	}
}

func TestAPIErrorsHideSensitiveServerBodies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		expected string
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "checkout", errors.New("token=server-secret")), "denied"},
		{"notfound", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "token=server-secret"), "no longer"},
		{"ratelimit", apierrors.NewTooManyRequests("token=server-secret", 0), "rate limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := fake.NewClientset()
			api.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
			result := NewWithClient(api, "test", "apps").ListResources(context.Background(), "pods", "", "", 50)
			if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], tc.expected) || strings.Contains(result.Errors[0], "server-secret") {
				t.Fatalf("unsafe error: %#v", result)
			}
		})
	}
}

func TestUnknownOrAmbiguousContainersDoNotReadLogs(t *testing.T) {
	pod := testPod("checkout", "apps")
	pod.Spec.InitContainers = []corev1.Container{{Name: "migration"}}
	api := fake.NewClientset(pod)
	b := NewWithClient(api, "test", "apps")
	for _, container := range []string{"", "unknown"} {
		result := b.PodLogs(context.Background(), "apps", "checkout", container, false)
		if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "app, migration") {
			t.Fatalf("bad selection error: %#v", result)
		}
	}
	for _, action := range api.Actions() {
		if action.GetVerb() != "get" || action.GetSubresource() != "" {
			t.Fatalf("unexpected action: %#v", action)
		}
	}
}

func httpBackend(t *testing.T, handler http.HandlerFunc) *Backend {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, Timeout: readTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return NewWithClient(api, "http-test", "apps")
}

func TestLogsAreBoundedRedactedAndReferenceVerifiedUID(t *testing.T) {
	var reads, logs atomic.Int32
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-read method %s", r.Method)
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			logs.Add(1)
			for key, value := range map[string]string{"container": "app", "previous": "true", "tailLines": "100", "limitBytes": "16000", "sinceSeconds": "900", "timestamps": "true"} {
				if r.URL.Query().Get(key) != value {
					t.Errorf("%s=%q, want %s", key, r.URL.Query().Get(key), value)
				}
			}
			if r.URL.Query().Get("follow") == "true" {
				t.Error("follow requested")
			}
			_, _ = w.Write([]byte("password=log-secret startup failed\n"))
			return
		}
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testPod("checkout", "apps"))
	})
	result := b.PodLogs(context.Background(), "apps", "checkout", "", true)
	if len(result.Errors) > 0 || len(result.Resources) != 1 {
		t.Fatalf("logs failed: %#v", result)
	}
	if result.Resources[0].Ref.UID != "uid-checkout" || result.Data["pod_uid"] != "uid-checkout" {
		t.Fatal("missing actual pod identity")
	}
	if strings.Contains(result.Data["logs"].(string), "log-secret") {
		t.Fatal("unredacted logs")
	}
	if reads.Load() != 2 || logs.Load() != 1 {
		t.Fatalf("reads=%d logs=%d", reads.Load(), logs.Load())
	}
}

func TestReplacedPodDiscardsLogs(t *testing.T) {
	var reads atomic.Int32
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			_, _ = w.Write([]byte("ambiguous content"))
			return
		}
		obj := testPod("checkout", "apps")
		if reads.Add(1) == 2 {
			obj.UID = "replacement"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj)
	})
	result := b.PodLogs(context.Background(), "apps", "checkout", "app", false)
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "replaced") || result.Data["logs"] != nil {
		t.Fatalf("ambiguous logs leaked: %#v", result)
	}
}

func TestLogServerIgnoringBoundsIsStillBounded(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			_, _ = w.Write([]byte(strings.Repeat("x", 100000)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testPod("checkout", "apps"))
	})
	result := b.PodLogs(context.Background(), "apps", "checkout", "app", false)
	if len(result.Errors) > 0 || len(result.Data["logs"].(string)) > logBytes || result.Data["truncated"] != true {
		t.Fatalf("unbounded logs: %#v", result.Data)
	}
}

func TestCallerDeadlineIsRespected(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := b.GetResource(ctx, "pods", "apps", "checkout")
	if len(result.Errors) == 0 || time.Since(start) > time.Second {
		t.Fatalf("caller deadline was ignored: %v", result)
	}
}

func TestListSendsBoundedPageWithoutServerSideQuery(t *testing.T) {
	var reads atomic.Int32
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path != "/api/v1/namespaces/apps/pods" || r.Method != http.MethodGet {
			t.Errorf("unexpected read %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("continue") != "" || r.URL.Query().Get("labelSelector") != "" {
			t.Errorf("unexpected list options: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			ListMeta: metav1.ListMeta{Continue: "more-pods"},
			Items:    []corev1.Pod{*testPod("checkout", "apps"), *testPod("other", "apps")},
		})
	})
	result := b.ListResources(context.Background(), "pods", "", "checkout", 2)
	if len(result.Errors) > 0 || len(result.Resources) != 1 || reads.Load() != 1 || result.Data["truncated"] != true {
		t.Fatalf("list paging failed: %#v", result)
	}
}

func TestGetRejectsMismatchedServerIdentity(t *testing.T) {
	api := fake.NewClientset()
	api.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, testPod("checkout", "other-namespace"), nil
	})
	result := NewWithClient(api, "test", "apps").GetResource(context.Background(), "pods", "apps", "checkout")
	if len(result.Errors) != 1 || len(result.Resources) != 0 {
		t.Fatalf("mismatched resource was returned: %#v", result)
	}
}

func TestUnavailablePreviousLogsReturnUsefulSanitizedError(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(&metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   "Failure", Code: 400, Reason: metav1.StatusReasonBadRequest,
				Message: "token=server-secret previous container not found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(testPod("checkout", "apps"))
	})
	result := b.PodLogs(context.Background(), "apps", "checkout", "app", true)
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "previous container logs") || strings.Contains(result.Errors[0], "server-secret") {
		t.Fatalf("unexpected log failure: %#v", result)
	}
}

func TestKubeconfigDoesNotChangeAndExplicitMissingDoesNotFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	contents := []byte("apiVersion: v1\nkind: Config\ncurrent-context: local\nclusters:\n- name: local\n  cluster:\n    server: https://127.0.0.1:6443\ncontexts:\n- name: local\n  context:\n    cluster: local\n    user: local\nusers:\n- name: local\n  user:\n    token: example-test-token\n")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := New("", "apps", path)
	if err != nil {
		t.Fatal(err)
	}
	if b.ContextName() != "local" || b.Namespace() != "apps" {
		t.Fatal("incorrect context or namespace")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(contents) {
		t.Fatal("kubeconfig was changed")
	}
	if _, err := New("", "apps", path+"-missing"); err == nil {
		t.Fatal("missing explicit kubeconfig accepted")
	}
	if _, err := New("unknown-context", "apps", path); err == nil {
		t.Fatal("unknown explicit context accepted")
	}
}
