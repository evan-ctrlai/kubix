package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/validation"

	"kubix/internal/core"
	"kubix/internal/redact"
)

const (
	crashLoopMaxReads    = 10
	crashLoopTimeout     = 60 * time.Second
	crashLoopEventLimit  = 30
	observationByteLimit = 64 * 1024
)

var _ core.CrashLoopCollector = (*Backend)(nil)

type crashCapture struct {
	backend  *Backend
	ctx      context.Context
	caseData *core.Investigation
	reads    int
	onEvent  func(string)
}

// CollectCrashLoop creates an immutable evidence case using a fixed read plan.
// The usual Pod -> ReplicaSet -> Deployment path requires seven API operations:
// three snapshots, two log samples, one UID-filtered event page, and a final pod
// identity check. Every read has a ten-second child deadline; the whole capture
// has a sixty-second deadline and obeys any shorter caller deadline.
func (b *Backend) CollectCrashLoop(ctx context.Context, target core.ResourceRef, container string, onEvent func(string)) *core.Investigation {
	caseData := core.NewInvestigation(b.contextName, target.Namespace, target)
	defer func() { caseData.FinishedAt = time.Now().UTC() }()
	ctx, cancel := context.WithTimeout(ctx, crashLoopTimeout)
	defer cancel()
	c := &crashCapture{backend: b, ctx: ctx, caseData: caseData, onEvent: onEvent}
	if target.Kind != "pods" {
		c.gap("Target pod", "kubernetes/pods", target, "", "CrashLoop investigations require a selected pod")
		return caseData
	}
	namespace, err := b.scope("pods", target.Namespace)
	if err != nil || namespace == "" || len(validation.IsDNS1123Subdomain(target.Name)) != 0 ||
		(container != "" && len(validation.IsDNS1123Label(container)) != 0) {
		message := "A valid pod name, namespace, and optional container are required"
		if err != nil {
			message = err.Error()
		}
		c.gap("Target pod", "kubernetes/pods", target, "", message)
		return caseData
	}
	target.Namespace = namespace
	caseData.Namespace, caseData.Target = namespace, target
	c.limit("Collection is sequential, not an atomic cluster snapshot; capture times and resource versions describe each read")
	c.limit("Only current sanitized workload state, a bounded event page, and sampled logs are collected; no deployment history or metrics are available")
	query := fmt.Sprintf("GET pod namespace=%s name=%s expected_uid=%s", namespace, target.Name, target.UID)
	pod, observation := captureRead(c, "Pod snapshot", "kubernetes/pods", target, query, func(ctx context.Context) (*corev1.Pod, error) {
		return b.client.CoreV1().Pods(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	})
	if pod == nil {
		c.add(observation)
		c.limit("Owner, log, and event reads were skipped because the target pod identity could not be established")
		return caseData
	}
	actual := resourceRef("pods", pod.ObjectMeta)
	if actual.Name != target.Name || actual.Namespace != namespace || actual.UID == "" || (target.UID != "" && target.UID != actual.UID) {
		c.changed(observation, target.UID, actual.UID, "Selected pod identity changed; refresh the resource list before investigating its replacement")
		return caseData
	}
	caseData.Target = actual
	observation.Ref, observation.ResourceVersion = actual, pod.ResourceVersion
	c.resource(observation, podResource(pod))
	c.owners(pod.ObjectMeta, actual, true)
	selected, selectionNote := failingContainer(pod, container)
	var pending []core.Observation
	if selected == "" {
		c.gap("Container selection", "kubernetes/pods", actual, "Choose a failing container", selectionNote)
	} else {
		for _, previous := range []bool{true, false} {
			mode := "Current"
			if previous {
				mode = "Previous"
			}
			logQuery := fmt.Sprintf("GET pod/log namespace=%s name=%s uid=%s container=%s previous=%t tail_lines=100 limit_bytes=16000 since_seconds=900 timestamps=true follow=false", namespace, pod.Name, pod.UID, selected, previous)
			logs, logObservation := captureRead(c, mode+" logs · "+selected, "kubernetes/pods/log", actual, logQuery, func(ctx context.Context) ([]byte, error) {
				tail, bytesLimit, since := int64(100), int64(logBytes), int64(900)
				stream, err := b.client.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
					Container: selected, Previous: previous, Follow: false, Timestamps: true,
					TailLines: &tail, LimitBytes: &bytesLimit, SinceSeconds: &since,
				}).Stream(ctx)
				if err != nil {
					return nil, err
				}
				defer stream.Close()
				return io.ReadAll(io.LimitReader(stream, logBytes+1))
			})
			logObservation.Format = "log"
			logObservation.ResourceVersion = pod.ResourceVersion
			if len(logObservation.Errors) == 0 {
				logObservation.State = "sampled"
				logObservation.Notes = append(logObservation.Notes, "Sample limited to the last 15 minutes, 100 lines, and 16 KB; older or rotated logs may be absent")
				if len(logs) > logBytes {
					logs = logs[:logBytes]
					logObservation.Notes = append(logObservation.Notes, "Server response exceeded the byte limit; extra content was discarded")
				}
				logObservation.Payload = boundedLogPayload(string(logs), &logObservation)
				if selectionNote != "" {
					logObservation.Notes = append(logObservation.Notes, selectionNote)
				}
			}
			pending = append(pending, logObservation)
		}
	}
	c.events(pod)
	// Logs cannot be tied to a UID in the API request itself. Do not publish them
	// as captured evidence until the pod and selected container are rechecked.
	final, identity := captureRead(c, "Pod identity verification", "kubernetes/pods", actual,
		fmt.Sprintf("GET pod namespace=%s name=%s expected_uid=%s", namespace, pod.Name, pod.UID),
		func(ctx context.Context) (*corev1.Pod, error) {
			return b.client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		})
	verified := final != nil && final.Name == pod.Name && final.Namespace == namespace && final.UID == pod.UID
	initialInstance, finalInstance := containerInstance(pod, selected), ""
	if verified {
		finalInstance = containerInstance(final, selected)
	}
	containerUnverified := selected != "" && (initialInstance == "" || finalInstance == "")
	containerChanged := verified && selected != "" && initialInstance != finalInstance
	if final == nil {
		c.add(identity)
		c.limit("The final pod identity read failed; log content was discarded because its resource identity could not be verified")
	} else if !verified {
		c.changed(identity, string(pod.UID), string(final.UID), "Pod was replaced during collection; log content was discarded")
	} else {
		identity.ResourceVersion = final.ResourceVersion
		identity.Payload = c.json(map[string]any{"expected_uid": pod.UID, "observed_uid": final.UID,
			"initial_resource_version": pod.ResourceVersion, "final_resource_version": final.ResourceVersion,
			"container": selected, "container_changed": containerChanged, "container_verified": selected != "" && !containerUnverified}, &identity)
		if pod.ResourceVersion != final.ResourceVersion || containerChanged {
			identity.State = "changed"
			identity.Notes = append(identity.Notes, "Pod state changed during collection; the original snapshot remains preserved")
		}
		if containerChanged {
			c.limit("The selected container restarted or changed during collection; log samples were discarded to avoid mixing container instances")
		}
		if containerUnverified {
			identity.State = "unavailable"
			identity.Errors = []string{"Pod UID was confirmed, but recorded container runtime identity was unavailable; log content was discarded"}
		}
		c.add(identity)
	}
	for _, item := range pending {
		if len(item.Errors) == 0 && (!verified || containerChanged || containerUnverified) {
			item.Payload = ""
			item.State = "changed"
			item.Errors = []string{"Log sample identity could not be verified against the initial pod/container; content discarded"}
		}
		c.add(item)
	}
	return caseData
}

func captureRead[T any](c *crashCapture, title, source string, ref core.ResourceRef, query string, read func(context.Context) (T, error)) (T, core.Observation) {
	var zero T
	observation := core.Observation{Title: title, Source: source, Ref: ref, Query: query, Format: "json", State: "unavailable"}
	if c.ctx.Err() != nil {
		observation.CapturedAt = time.Now().UTC()
		observation.Errors = []string{"Read skipped: investigation canceled or its time budget expired"}
		return zero, observation
	}
	if c.reads >= crashLoopMaxReads {
		observation.CapturedAt = time.Now().UTC()
		observation.Errors = []string{"Read skipped: investigation API operation budget exhausted"}
		return zero, observation
	}
	c.reads++
	if c.onEvent != nil {
		c.onEvent(title)
	}
	ctx, cancel := context.WithTimeout(c.ctx, readTimeout)
	defer cancel()
	value, err := read(ctx)
	observation.CapturedAt = time.Now().UTC()
	if err != nil {
		observation.Errors = failure("collect_crashloop", err).Errors
		return zero, observation
	}
	observation.State = "captured"
	return value, observation
}

func (c *crashCapture) limit(message string) {
	message = redact.Text(message, 1000)
	for _, existing := range c.caseData.Limitations {
		if existing == message {
			return
		}
	}
	c.caseData.Limitations = append(c.caseData.Limitations, message)
}

func (c *crashCapture) add(observation core.Observation) {
	for n, message := range observation.Errors {
		observation.Errors[n] = redact.Text(message, 1000)
		c.limit(observation.Title + ": " + observation.Errors[n])
	}
	c.caseData.AddObservation(observation)
}

func (c *crashCapture) gap(title, source string, ref core.ResourceRef, query, message string) {
	c.add(core.Observation{Title: title, Source: source, Ref: ref, Query: query,
		State: "unavailable", Format: "json", Errors: []string{message}})
}

func (c *crashCapture) changed(observation core.Observation, expectedUID, observedUID, message string) {
	observation.State = "changed"
	observation.Errors = []string{message}
	observation.Payload = c.json(map[string]any{"expected_uid": expectedUID, "observed_uid": observedUID}, &observation)
	c.add(observation)
}

func (c *crashCapture) json(value any, observation *core.Observation) string {
	encoded, err := json.Marshal(value)
	if err == nil && len(encoded) <= observationByteLimit {
		return string(encoded)
	}
	observation.State = "sampled"
	observation.Notes = append(observation.Notes, "Structured data exceeded the 64 KiB observation limit or could not be encoded; payload omitted")
	c.limit(observation.Title + ": structured payload omitted because of the capture limit")
	return `{"omitted":true,"reason":"Structured evidence exceeded the capture limit"}`
}

func (c *crashCapture) resource(observation core.Observation, resource core.Resource) {
	observation.Ref = resource.Ref
	observation.Notes = append(observation.Notes, "Sanitized field subset; environment values, commands, arbitrary annotations, and Secret contents are excluded")
	if encoded, err := json.Marshal(resource); err != nil || len(encoded) > observationByteLimit {
		// Bound the browser copy as well as the serialized observation. Keeping a
		// large Details map here would bypass the evidence payload limit.
		resource.Details = map[string]any{"omitted": true, "reason": "Sanitized resource details exceeded the 64 KiB capture limit"}
		observation.State = "sampled"
		observation.Notes = append(observation.Notes, "Resource details exceeded the capture limit; only identity, status, and summary were retained")
		c.limit(observation.Title + ": resource details were omitted because of the capture limit")
	}
	observation.Payload = c.json(resource, &observation)
	c.add(observation)
	for _, existing := range c.caseData.Resources {
		if existing.Ref.Key() == resource.Ref.Key() {
			return
		}
	}
	c.caseData.Resources = append(c.caseData.Resources, resource)
}

func (c *crashCapture) owners(meta metav1.ObjectMeta, child core.ResourceRef, allowReplica bool) {
	controllers := []metav1.OwnerReference{}
	for _, owner := range meta.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			controllers = append(controllers, owner)
		}
	}
	if len(controllers) != 1 {
		message := "No controller owner is recorded; this may be a standalone pod or ReplicaSet"
		if len(controllers) > 1 {
			message = "Multiple controller owners are recorded; owner traversal was skipped because it is ambiguous"
		}
		c.gap("Workload owner", "kubernetes/owners", child, "Follow controller ownerReference UID", message)
		return
	}
	owner := controllers[0]
	if owner.UID == "" || len(validation.IsDNS1123Subdomain(owner.Name)) != 0 || owner.APIVersion != "apps/v1" {
		c.gap("Workload owner", "kubernetes/owners", child, "Follow controller ownerReference UID", "Controller owner identity or API version is unsupported; no owner read was attempted")
		return
	}
	ref := core.ResourceRef{Namespace: child.Namespace, Name: owner.Name, UID: string(owner.UID)}
	query := fmt.Sprintf("GET owner namespace=%s name=%s expected_uid=%s", ref.Namespace, ref.Name, ref.UID)
	switch owner.Kind {
	case "ReplicaSet":
		if !allowReplica {
			c.gap("Workload owner", "kubernetes/owners", child, query, "Unexpected nested ReplicaSet owner; traversal stopped")
			return
		}
		ref.Kind = "replicasets"
		obj, observation := captureRead(c, "ReplicaSet owner", "kubernetes/replicasets", ref, query, func(ctx context.Context) (*appsv1.ReplicaSet, error) {
			return c.backend.client.AppsV1().ReplicaSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		})
		if obj == nil {
			c.add(observation)
			c.limit("Deployment traversal was skipped because the ReplicaSet owner could not be read")
			return
		}
		if obj.UID != owner.UID || obj.Name != ref.Name || obj.Namespace != ref.Namespace {
			c.changed(observation, ref.UID, string(obj.UID), "ReplicaSet owner identity differs from the captured Pod ownerReference; traversal stopped")
			return
		}
		observation.ResourceVersion = obj.ResourceVersion
		c.resource(observation, replicaSetResource(obj))
		c.owners(obj.ObjectMeta, ref, false)
	case "Deployment":
		ref.Kind = "deployments"
		obj, observation := captureRead(c, "Deployment owner", "kubernetes/deployments", ref, query, func(ctx context.Context) (*appsv1.Deployment, error) {
			return c.backend.client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		})
		if obj == nil {
			c.add(observation)
			return
		}
		if obj.UID != owner.UID || obj.Name != ref.Name || obj.Namespace != ref.Namespace {
			c.changed(observation, ref.UID, string(obj.UID), "Deployment owner identity differs from the captured controller reference; traversal stopped")
			return
		}
		observation.ResourceVersion = obj.ResourceVersion
		c.resource(observation, deploymentResource(obj))
	default:
		c.gap("Workload owner", "kubernetes/owners", child, query, "Controller owner kind "+redact.Text(owner.Kind, 80)+" is not collected in this version")
	}
}

func (c *crashCapture) events(pod *corev1.Pod) {
	ref := resourceRef("pods", pod.ObjectMeta)
	selector := fields.OneTermEqualSelector("involvedObject.uid", string(pod.UID)).String()
	query := fmt.Sprintf("LIST events namespace=%s field_selector=%s limit=%d", pod.Namespace, selector, crashLoopEventLimit)
	list, observation := captureRead(c, "Pod events", "kubernetes/events", ref, query, func(ctx context.Context) (*corev1.EventList, error) {
		return c.backend.client.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector, Limit: crashLoopEventLimit})
	})
	if list == nil {
		c.add(observation)
		return
	}
	events := []core.Resource{}
	skipped := 0
	for _, event := range list.Items[:min(len(list.Items), crashLoopEventLimit)] {
		if event.InvolvedObject.UID != pod.UID || event.Namespace != pod.Namespace ||
			(event.InvolvedObject.Namespace != "" && event.InvolvedObject.Namespace != pod.Namespace) {
			skipped++
			continue
		}
		event.Message = redact.Text(event.Message, 1000)
		resource := eventResource(&event)
		if validRef(resource.Ref) {
			events = append(events, resource)
		}
	}
	truncated := list.Continue != "" || len(list.Items) > crashLoopEventLimit
	observation.State, observation.ResourceVersion = "sampled", list.ResourceVersion
	observation.Notes = append(observation.Notes, "Events are a best-effort bounded page with limited retention; an empty sample does not prove no failure occurred")
	if truncated {
		observation.Notes = append(observation.Notes, "More event pages exist; they were not fetched")
		c.limit("Pod event history is incomplete because the bounded page was truncated")
	}
	if skipped > 0 {
		observation.Notes = append(observation.Notes, "Events with unrelated UID or namespace were discarded")
	}
	observation.Payload = c.json(map[string]any{"events": events, "matched_count": len(events), "truncated": truncated, "selector": selector}, &observation)
	c.add(observation)
}

func failingContainer(pod *corev1.Pod, selected string) (string, string) {
	names := map[string]bool{}
	for _, item := range pod.Spec.Containers {
		names[item.Name] = true
	}
	for _, item := range pod.Spec.InitContainers {
		names[item.Name] = true
	}
	states := append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...)
	crashing := []string{}
	for _, item := range states {
		if names[item.Name] && item.State.Waiting != nil && item.State.Waiting.Reason == "CrashLoopBackOff" {
			crashing = append(crashing, item.Name)
		}
	}
	if selected != "" {
		if !names[selected] {
			return "", "Selected container is absent from the captured pod; no logs were read"
		}
		for _, name := range crashing {
			if name == selected {
				return selected, ""
			}
		}
		return selected, "Selected container was not currently waiting in CrashLoopBackOff; the incident may have recovered or changed"
	}
	if len(crashing) == 1 {
		return crashing[0], ""
	}
	if len(crashing) > 1 {
		return "", "Multiple containers are in CrashLoopBackOff; select one explicitly: " + strings.Join(crashing[:min(len(crashing), 20)], ", ")
	}
	return "", "No container is currently in CrashLoopBackOff; choose a container explicitly if historical logs are needed"
}

func containerInstance(pod *corev1.Pod, name string) string {
	states := append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...)
	for _, item := range states {
		if item.Name != name {
			continue
		}
		last, current, started := "", "", ""
		identifiable := item.ContainerID != "" || item.RestartCount > 0
		if item.LastTerminationState.Terminated != nil {
			terminated := item.LastTerminationState.Terminated
			last = terminated.ContainerID + "/" + terminated.FinishedAt.Time.Format(time.RFC3339Nano)
			identifiable = identifiable || terminated.ContainerID != "" || !terminated.FinishedAt.IsZero()
		}
		if item.State.Terminated != nil {
			terminated := item.State.Terminated
			current = terminated.ContainerID + "/" + terminated.FinishedAt.Time.Format(time.RFC3339Nano)
			identifiable = identifiable || terminated.ContainerID != "" || !terminated.FinishedAt.IsZero()
		}
		if item.State.Running != nil && !item.State.Running.StartedAt.IsZero() {
			started = item.State.Running.StartedAt.Time.Format(time.RFC3339Nano)
			identifiable = true
		}
		if !identifiable {
			return ""
		}
		return fmt.Sprintf("%s/%d/%s/%s/%s", item.ContainerID, item.RestartCount, last, current, started)
	}
	return ""
}

// API limits are requested, then enforced locally as well. Redaction can expand
// short values, so bound its result without inserting text into a cited log line.
func boundedLogPayload(raw string, observation *core.Observation) string {
	payload := redact.Text(raw, -1)
	lines := strings.Split(strings.TrimSuffix(payload, "\n"), "\n")
	if len(lines) > 100 {
		payload = strings.Join(lines[:100], "\n")
		observation.Notes = append(observation.Notes, "Server returned more than 100 lines; extra lines were discarded")
	}
	if len(payload) > logBytes {
		payload = payload[:logBytes]
		for !utf8.ValidString(payload) {
			payload = payload[:len(payload)-1]
		}
		observation.Notes = append(observation.Notes, "Redacted log text exceeded 16 KB; extra content was discarded")
	}
	return payload
}
