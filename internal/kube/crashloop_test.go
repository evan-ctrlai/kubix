package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"kubix/internal/core"
)

const crashPodPath = "/api/v1/namespaces/apps/pods/checkout"
const crashRSPath = "/apis/apps/v1/namespaces/apps/replicasets/checkout-rs"
const crashDeploymentPath = "/apis/apps/v1/namespaces/apps/deployments/checkout"
const crashEventsPath = "/api/v1/namespaces/apps/events"

type crashRequest struct {
	method, path string
	query        url.Values
}

type crashFixture struct {
	pod        *corev1.Pod
	replica    *appsv1.ReplicaSet
	deployment *appsv1.Deployment
	events     *corev1.EventList
	finalPod   func(*corev1.Pod) *corev1.Pod
	errors     map[string]int
	previous   string
	current    string
	mu         sync.Mutex
	requests   []crashRequest
	podReads   int
}

func controller(kind, name, uid string) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: &yes}
}

func newCrashFixture(t *testing.T) (*Backend, *crashFixture) {
	t.Helper()
	pod := testPod("checkout", "apps")
	pod.ResourceVersion = "42"
	pod.OwnerReferences = []metav1.OwnerReference{controller("ReplicaSet", "checkout-rs", "uid-rs")}
	f := &crashFixture{
		pod: pod,
		replica: &appsv1.ReplicaSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
			ObjectMeta: metav1.ObjectMeta{Name: "checkout-rs", Namespace: "apps", UID: "uid-rs", ResourceVersion: "43",
				OwnerReferences: []metav1.OwnerReference{controller("Deployment", "checkout", "uid-deployment")},
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": "3", "secret": "rs-secret-sentinel"}},
			Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: pod.Spec}}},
		deployment: &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "apps", UID: "uid-deployment", ResourceVersion: "44"},
			Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: pod.Spec}}},
		events: &corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"},
			ListMeta: metav1.ListMeta{ResourceVersion: "45"}, Items: []corev1.Event{{
				ObjectMeta:     metav1.ObjectMeta{Name: "checkout.event", Namespace: "apps", UID: "uid-event"},
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "checkout", Namespace: "apps", UID: pod.UID},
				Type:           "Warning", Reason: "BackOff", Message: "password=event-secret back-off restarting failed container", Count: 4,
			}}},
		errors: map[string]int{}, previous: "2026-11-13T12:00:00Z password=log-secret\n2026-11-13T12:00:01Z out of memory\n",
		current: "2026-11-13T12:01:00Z starting\n",
	}
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, crashRequest{r.Method, r.URL.Path, r.URL.Query()})
		key := r.URL.Path
		if key == crashPodPath {
			f.podReads++
			if f.podReads > 1 {
				key += "#final"
			}
		}
		if strings.HasSuffix(r.URL.Path, "/log") && r.URL.Query().Get("previous") == "true" {
			key += "#previous"
		}
		if code := f.errors[key]; code != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status: "Failure", Code: int32(code), Message: "opaque-server-secret-must-not-leak"})
			return
		}
		if r.URL.Path == crashPodPath+"/log" {
			w.Header().Set("Content-Type", "text/plain")
			payload := f.current
			if r.URL.Query().Get("previous") == "true" {
				payload = f.previous
			}
			_, _ = w.Write([]byte(payload))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch r.URL.Path {
		case crashPodPath:
			response = f.pod
			if f.podReads > 1 && f.finalPod != nil {
				response = f.finalPod(f.pod.DeepCopy())
			}
		case crashRSPath:
			response = f.replica
		case crashDeploymentPath:
			response = f.deployment
		case crashEventsPath:
			response = f.events
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	return b, f
}

func captureFixture(b *Backend, container string) *core.Investigation {
	return b.CollectCrashLoop(context.Background(), core.ResourceRef{Kind: "pods", Namespace: "apps", Name: "checkout", UID: "uid-checkout"}, container, nil)
}

func findObservation(t *testing.T, investigation *core.Investigation, title string) core.Observation {
	t.Helper()
	for _, observation := range investigation.Observations {
		if observation.Title == title {
			return observation
		}
	}
	t.Fatalf("missing observation %q in %#v", title, investigation.Observations)
	return core.Observation{}
}

func TestCrashLoopCapturesVerifiedBoundedReadPlan(t *testing.T) {
	b, fixture := newCrashFixture(t)
	// A server-side filter is requested and independently checked locally.
	unrelated := fixture.events.Items[0].DeepCopy()
	unrelated.InvolvedObject.UID = "different-pod"
	unrelated.Message = "unrelated-event-must-not-leak"
	fixture.events.Items = append(fixture.events.Items, *unrelated)
	fixture.events.Continue = "opaque-next-page"
	investigation := captureFixture(b, "")
	if len(fixture.requests) != 7 || len(investigation.Resources) != 3 || len(investigation.Observations) != 7 {
		t.Fatalf("unexpected capture sizes: requests=%d resources=%d observations=%d", len(fixture.requests), len(investigation.Resources), len(investigation.Observations))
	}
	if investigation.Context != "http-test" || investigation.Target.UID != "uid-checkout" || investigation.FinishedAt.Before(investigation.StartedAt) {
		t.Fatalf("missing capture provenance: %#v", investigation)
	}
	logReads := 0
	for _, request := range fixture.requests {
		if request.method != http.MethodGet {
			t.Errorf("mutating request: %#v", request)
		}
		if request.path == crashPodPath+"/log" {
			logReads++
			for key, expected := range map[string]string{"container": "app", "tailLines": "100", "limitBytes": "16000", "sinceSeconds": "900", "timestamps": "true"} {
				if request.query.Get(key) != expected {
					t.Errorf("log bound %s: %q", key, request.query.Get(key))
				}
			}
			if request.query.Get("follow") == "true" {
				t.Error("streaming logs requested")
			}
		}
		if request.path == crashEventsPath && (request.query.Get("fieldSelector") != "involvedObject.uid=uid-checkout" || request.query.Get("limit") != "30" || request.query.Get("continue") != "") {
			t.Errorf("unbounded/unscoped event request: %#v", request)
		}
	}
	if logReads != 2 {
		t.Fatalf("logs read %d times", logReads)
	}
	for _, observation := range investigation.Observations {
		if observation.CapturedAt.IsZero() || observation.Query == "" || !strings.HasPrefix(observation.Source, "kubernetes/") || observation.ResourceVersion == "" {
			t.Errorf("incomplete read provenance: %#v", observation)
		}
		if observation.Format == "json" && !json.Valid([]byte(observation.Payload)) {
			t.Errorf("invalid captured JSON: %s", observation.Payload)
		}
	}
	for _, citation := range investigation.Citations {
		if _, _, ok := investigation.ResolveCitation(citation.ID); !ok {
			t.Errorf("broken citation %s", citation.ID)
		}
	}
	previous := findObservation(t, investigation, "Previous logs · app")
	if previous.State != "sampled" || !strings.Contains(previous.Payload, "out of memory") || !strings.Contains(previous.Query, "previous=true") {
		t.Fatalf("missing prior log sample: %#v", previous)
	}
	events := findObservation(t, investigation, "Pod events")
	var payload struct {
		Events       []core.Resource `json:"events"`
		MatchedCount int             `json:"matched_count"`
		Truncated    bool            `json:"truncated"`
	}
	if json.Unmarshal([]byte(events.Payload), &payload) != nil || len(payload.Events) != 1 || payload.MatchedCount != 1 || !payload.Truncated || events.State != "sampled" {
		t.Fatalf("incorrect event sample: %s", events.Payload)
	}
	encoded, _ := json.Marshal(investigation)
	for _, secret := range []string{"annotation-must-not-leak", "env-must-not-leak", "arg-must-not-leak", "state-secret", "rs-secret-sentinel", "event-secret", "log-secret", "unrelated-event-must-not-leak", "opaque-next-page"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("captured excluded value %s", secret)
		}
	}
}

func TestCrashLoopRejectsInvalidScopeBeforeAPI(t *testing.T) {
	api := fake.NewClientset()
	b := NewWithClient(api, "test", "apps")
	for _, ref := range []core.ResourceRef{
		{Kind: "deployments", Namespace: "apps", Name: "checkout"},
		{Kind: "pods", Namespace: "other", Name: "checkout"},
		{Kind: "pods", Namespace: "apps", Name: "../secrets"},
		{Kind: "pods", Namespace: "*", Name: "checkout"},
	} {
		investigation := b.CollectCrashLoop(context.Background(), ref, "", nil)
		if len(investigation.Observations) != 1 || investigation.Observations[0].State != "unavailable" {
			t.Fatalf("invalid target not rejected: %#v", investigation)
		}
	}
	b.CollectCrashLoop(context.Background(), core.ResourceRef{Kind: "pods", Name: "checkout"}, "../container", nil)
	if len(api.Actions()) != 0 {
		t.Fatalf("invalid target reached API: %#v", api.Actions())
	}
}

func TestCrashLoopRejectsSelectedUIDReplacementBeforeDependentReads(t *testing.T) {
	b, fixture := newCrashFixture(t)
	fixture.pod.UID = "new-pod"
	investigation := captureFixture(b, "")
	if len(fixture.requests) != 1 || len(investigation.Resources) != 0 || len(investigation.Observations) != 1 || investigation.Observations[0].State != "changed" {
		t.Fatalf("selected UID mismatch continued collecting: %#v", investigation)
	}
}

func TestCrashLoopPermissionAndMissingLogGapsPreserveOtherEvidence(t *testing.T) {
	b, fixture := newCrashFixture(t)
	fixture.errors[crashRSPath] = http.StatusForbidden
	fixture.errors[crashPodPath+"/log#previous"] = http.StatusBadRequest
	fixture.errors[crashEventsPath] = http.StatusForbidden
	investigation := captureFixture(b, "")
	for _, title := range []string{"ReplicaSet owner", "Previous logs · app", "Pod events"} {
		observation := findObservation(t, investigation, title)
		if observation.State != "unavailable" || len(observation.Errors) == 0 || observation.Payload != "" {
			t.Errorf("missing explicit failure gap: %#v", observation)
		}
	}
	if findObservation(t, investigation, "Current logs · app").State != "sampled" || len(investigation.Resources) != 1 {
		t.Fatalf("unrelated evidence was lost: %#v", investigation)
	}
	for _, request := range fixture.requests {
		if request.path == crashDeploymentPath {
			t.Error("traversed through unreadable ReplicaSet")
		}
	}
	encoded, _ := json.Marshal(investigation)
	if strings.Contains(string(encoded), "opaque-server-secret") || !strings.Contains(string(encoded), "403") {
		t.Fatalf("API errors leaked or lost status: %s", encoded)
	}
}

func TestExplicitHealthyContainerCapturesCurrentAndUnavailablePreviousLogs(t *testing.T) {
	b, fixture := newCrashFixture(t)
	started := metav1.NewTime(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	fixture.pod.Status.Phase = corev1.PodRunning
	fixture.pod.Status.ContainerStatuses[0].Ready = true
	fixture.pod.Status.ContainerStatuses[0].RestartCount = 0
	fixture.pod.Status.ContainerStatuses[0].ContainerID = "containerd://app-current"
	fixture.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}
	fixture.pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	fixture.current = "current healthy log sample\n"
	fixture.errors[crashPodPath+"/log#previous"] = http.StatusBadRequest

	investigation := captureFixture(b, "app")
	current := findObservation(t, investigation, "Current logs · app")
	previous := findObservation(t, investigation, "Previous logs · app")
	if current.State != "sampled" || !strings.Contains(current.Payload, "current healthy log sample") {
		t.Fatalf("explicit healthy-container current logs were not captured: %#v", current)
	}
	if previous.State != "unavailable" || previous.Payload != "" || len(previous.Errors) == 0 || !strings.Contains(strings.Join(previous.Errors, " "), "previous container logs may not exist") {
		t.Fatalf("unavailable previous logs were not preserved as an evidence gap: %#v", previous)
	}
	if !strings.Contains(strings.Join(investigation.Limitations, " "), "previous container logs") {
		t.Fatalf("missing limitation for unavailable previous logs: %#v", investigation.Limitations)
	}
	for _, citation := range investigation.Citations {
		if _, _, ok := investigation.ResolveCitation(citation.ID); !ok {
			t.Fatalf("broken citation %s", citation.ID)
		}
	}
}

func TestCrashLoopOwnerUIDMismatchStopsTraversal(t *testing.T) {
	b, fixture := newCrashFixture(t)
	fixture.replica.UID = "replacement-rs"
	investigation := captureFixture(b, "")
	if findObservation(t, investigation, "ReplicaSet owner").State != "changed" || len(investigation.Resources) != 1 {
		t.Fatalf("mismatched owner captured as fact: %#v", investigation)
	}
	for _, request := range fixture.requests {
		if request.path == crashDeploymentPath {
			t.Error("traversed mismatched owner")
		}
	}
}

func TestCrashLoopDirectAndUnsupportedOwnerPaths(t *testing.T) {
	for _, mode := range []string{"direct-deployment", "deployment-replaced", "unsupported-kind"} {
		t.Run(mode, func(t *testing.T) {
			b, fixture := newCrashFixture(t)
			fixture.pod.OwnerReferences = []metav1.OwnerReference{controller("Deployment", "checkout", "uid-deployment")}
			if mode == "unsupported-kind" {
				fixture.pod.OwnerReferences[0].Kind = "StatefulSet"
			} else if mode == "deployment-replaced" {
				fixture.deployment.UID = "replacement-deployment"
			}
			investigation := captureFixture(b, "")
			for _, request := range fixture.requests {
				if request.path == crashRSPath {
					t.Error("guessed a ReplicaSet rather than following the recorded controller")
				}
			}
			if mode == "direct-deployment" {
				if findObservation(t, investigation, "Deployment owner").State != "captured" || len(investigation.Resources) != 2 {
					t.Fatal("direct Deployment owner not captured")
				}
			} else if mode == "deployment-replaced" {
				if findObservation(t, investigation, "Deployment owner").State != "changed" || len(investigation.Resources) != 1 {
					t.Fatal("replacement Deployment accepted as the recorded owner")
				}
			} else if findObservation(t, investigation, "Workload owner").State != "unavailable" {
				t.Fatal("unsupported owner was not marked as a gap")
			}
		})
	}
}

func TestCrashLoopAmbiguousOwnersAndContainersAreExplicit(t *testing.T) {
	b, fixture := newCrashFixture(t)
	fixture.pod.OwnerReferences = append(fixture.pod.OwnerReferences, controller("ReplicaSet", "second", "uid-second"))
	fixture.pod.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "setup:v1"}}
	fixture.pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "setup", RestartCount: 2, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	investigation := captureFixture(b, "")
	if findObservation(t, investigation, "Workload owner").State != "unavailable" || findObservation(t, investigation, "Container selection").State != "unavailable" {
		t.Fatal("ambiguity was not exposed")
	}
	if len(fixture.requests) != 3 {
		t.Fatalf("ambiguous selection performed dependent reads: %#v", fixture.requests)
	}
	// Explicit selection resolves only container ambiguity; logs stay scoped to it.
	fixture.requests = nil
	fixture.podReads = 0
	investigation = captureFixture(b, "setup")
	if findObservation(t, investigation, "Previous logs · setup").State != "sampled" {
		t.Fatal("explicit init-container selection failed")
	}
	for _, request := range fixture.requests {
		if request.path == crashPodPath+"/log" && request.query.Get("container") != "setup" {
			t.Errorf("read unrelated container: %#v", request)
		}
	}
}

func TestCrashLoopFinalIdentityFailuresDiscardLogContent(t *testing.T) {
	for _, mode := range []string{"replacement", "restart", "verification-denied"} {
		t.Run(mode, func(t *testing.T) {
			b, fixture := newCrashFixture(t)
			fixture.finalPod = func(pod *corev1.Pod) *corev1.Pod {
				if mode == "replacement" {
					pod.UID = "replacement-pod"
				} else if mode == "restart" {
					pod.Status.ContainerStatuses[0].RestartCount++
				}
				return pod
			}
			if mode == "verification-denied" {
				fixture.errors[crashPodPath+"#final"] = http.StatusForbidden
			}
			investigation := captureFixture(b, "")
			for _, title := range []string{"Previous logs · app", "Current logs · app"} {
				observation := findObservation(t, investigation, title)
				if observation.State != "changed" || observation.Payload != "" || len(observation.Errors) == 0 {
					t.Errorf("ambiguous logs retained: %#v", observation)
				}
			}
			if findObservation(t, investigation, "Pod snapshot").State != "captured" {
				t.Error("initial snapshot was overwritten")
			}
		})
	}
}

func TestCrashLoopMissingRuntimeIdentityDiscardsExplicitContainerLogs(t *testing.T) {
	for _, mode := range []string{"missing-status", "empty-runtime-status"} {
		t.Run(mode, func(t *testing.T) {
			b, fixture := newCrashFixture(t)
			fixture.pod.Status.ContainerStatuses = nil
			if mode == "empty-runtime-status" {
				fixture.pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app"}}
			}
			investigation := captureFixture(b, "app")
			for _, title := range []string{"Previous logs · app", "Current logs · app"} {
				observation := findObservation(t, investigation, title)
				if observation.State != "changed" || observation.Payload != "" {
					t.Errorf("logs accepted without comparable runtime state: %#v", observation)
				}
			}
			verification := findObservation(t, investigation, "Pod identity verification")
			if verification.State != "unavailable" || !strings.Contains(strings.Join(verification.Errors, ""), "runtime identity") {
				t.Errorf("missing runtime verification gap: %#v", verification)
			}
		})
	}
}

func TestCrashLoopEnforcesOutputBoundsLocally(t *testing.T) {
	b, fixture := newCrashFixture(t)
	fixture.previous = strings.Repeat("password=x ", 3000)
	fixture.current = strings.Repeat("a short line\n", 150)
	investigation := captureFixture(b, "")
	previous := findObservation(t, investigation, "Previous logs · app")
	current := findObservation(t, investigation, "Current logs · app")
	if len(previous.Payload) > logBytes || strings.Contains(previous.Payload, "password=x") || len(previous.Notes) < 2 {
		t.Fatalf("raw/redacted byte bound not enforced: len=%d notes=%v", len(previous.Payload), previous.Notes)
	}
	if len(strings.Split(strings.TrimSuffix(current.Payload, "\n"), "\n")) > 100 || len(current.Notes) < 2 {
		t.Fatalf("line bound not enforced: %#v", current)
	}
}

func TestCrashLoopBoundsStructuredEvidenceAndBrowserCopy(t *testing.T) {
	investigation := core.NewInvestigation("test", "apps", core.ResourceRef{})
	capture := &crashCapture{caseData: investigation}
	resource := core.Resource{Ref: core.ResourceRef{Kind: "pods", Namespace: "apps", Name: "checkout"},
		Status: "CrashLoopBackOff", Details: map[string]any{"many_fields": strings.Repeat("x", observationByteLimit)}}
	capture.resource(core.Observation{Title: "Pod snapshot", Format: "json", State: "captured"}, resource)
	observation := investigation.Observations[0]
	if len(observation.Payload) > observationByteLimit || !json.Valid([]byte(observation.Payload)) || observation.State != "sampled" || investigation.Resources[0].Details["omitted"] != true {
		t.Fatal("oversized details escaped through evidence or browser resources")
	}
}

func TestFailingContainerChoosesActualCrashAcrossRegularAndInitContainers(t *testing.T) {
	pod := testPod("checkout", "apps")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "healthy"})
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: "healthy", Ready: true})
	if selected, note := failingContainer(pod, ""); selected != "app" || note != "" {
		t.Fatalf("unrelated container prevented selecting unique crash: %s %s", selected, note)
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{}
	pod.Spec.InitContainers = []corev1.Container{{Name: "setup"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "setup", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	if selected, note := failingContainer(pod, ""); selected != "setup" || note != "" {
		t.Fatalf("init crash was not selected: %s %s", selected, note)
	}
	if selected, note := failingContainer(pod, "missing"); selected != "" || note == "" {
		t.Fatal("unknown explicit container accepted")
	}
}

func TestCrashLoopCancellationBudgetAndReadDeadlines(t *testing.T) {
	api := fake.NewClientset()
	b := NewWithClient(api, "test", "apps")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	investigation := b.CollectCrashLoop(ctx, core.ResourceRef{Kind: "pods", Name: "checkout"}, "", nil)
	if len(api.Actions()) != 0 || findObservation(t, investigation, "Pod snapshot").State != "unavailable" {
		t.Fatal("canceled investigation called API")
	}
	capture := &crashCapture{ctx: context.Background(), reads: crashLoopMaxReads}
	called := false
	_, observation := captureRead(capture, "Read", "kubernetes/pods", core.ResourceRef{}, "GET", func(context.Context) (bool, error) {
		called = true
		return true, nil
	})
	if called || observation.State != "unavailable" || !strings.Contains(strings.Join(observation.Errors, ""), "budget") {
		t.Fatal("API budget was not enforced")
	}
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	capture.ctx, capture.reads = parent, 0
	_, _ = captureRead(capture, "Read", "kubernetes/pods", core.ResourceRef{}, "GET", func(ctx context.Context) (bool, error) {
		deadline, exists := ctx.Deadline()
		if !exists || time.Until(deadline) > time.Second {
			t.Error("caller deadline not preserved")
		}
		return true, nil
	})
}

func TestReplicaSetBrowserReadsRemainSanitizedAndScoped(t *testing.T) {
	_, fixture := newCrashFixture(t)
	api := fake.NewClientset(fixture.replica)
	b := NewWithClient(api, "test", "apps")
	listed := b.ListResources(context.Background(), "replicasets", "", "checkout", 1)
	inspected := b.GetResource(context.Background(), "replicasets", "apps", "checkout-rs")
	if len(listed.Resources) != 1 || len(inspected.Resources) != 1 || inspected.Resources[0].Ref.UID != "uid-rs" {
		t.Fatalf("ReplicaSet browsing failed: %#v %#v", listed, inspected)
	}
	encoded, _ := json.Marshal(inspected)
	if strings.Contains(string(encoded), "rs-secret-sentinel") || strings.Contains(string(encoded), "env-must-not-leak") || inspected.Resources[0].Details["revision"] != "3" {
		t.Fatalf("ReplicaSet fields not whitelisted: %s", encoded)
	}
	for _, action := range api.Actions() {
		if (action.GetVerb() != "get" && action.GetVerb() != "list") || action.GetNamespace() != "apps" || action.GetResource().Resource != "replicasets" {
			t.Errorf("unexpected operation: %#v", action)
		}
	}
}
