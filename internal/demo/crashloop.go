package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubix/internal/core"
	"kubix/internal/redact"
)

var _ core.CrashLoopCollector = (*Backend)(nil)

// CollectCrashLoop uses the offline fixtures to exercise the same captured
// evidence workflow as a cluster. It never initializes a Kubernetes client.
func (b *Backend) CollectCrashLoop(ctx context.Context, target core.ResourceRef, container string, onEvent func(string)) *core.Investigation {
	if target.Namespace == "" {
		target.Namespace = b.Namespace()
	}
	i := core.NewInvestigation(b.ContextName(), target.Namespace, target)
	i.Limitations = []string{"This capture is kept in memory for this session; it is not a historical or atomic cluster snapshot."}
	defer func() { i.FinishedAt = time.Now().UTC() }()
	notify := func(message string) {
		if onEvent != nil {
			onEvent(message)
		}
	}
	query := func(ref core.ResourceRef) string {
		return fmt.Sprintf("GET %s namespace=%s name=%s expected_uid=%s", ref.Kind, ref.Namespace, ref.Name, ref.UID)
	}
	gap := func(title, source string, ref core.ResourceRef, reason string) {
		i.AddObservation(core.Observation{Title: title, Source: source, Ref: ref, Query: query(ref), State: "unavailable", Format: "json", Errors: []string{redact.Text(reason, 2000)}})
	}
	captureResource := func(resource core.Resource, title string) {
		payload, _ := json.Marshal(redact.Value(resource, 2000))
		i.Resources = append(i.Resources, clone(resource))
		i.AddObservation(core.Observation{Title: title, Source: "kubernetes/" + resource.Ref.Kind, Ref: resource.Ref, ResourceVersion: "demo-fixture-1", Query: query(resource.Ref), State: "captured", Format: "json", Payload: string(payload), Notes: []string{"Synthetic fixture; configuration values and Secrets are not collected."}})
	}
	if target.Kind != "pods" || target.Name == "" || target.Namespace != b.Namespace() {
		gap("Pod", "kubernetes/pods", target, "Choose a named pod in the demo namespace")
		return i
	}
	notify("Capturing pod identity and container state")
	result := b.GetResource(ctx, "pods", target.Namespace, target.Name)
	if len(result.Errors) > 0 || len(result.Resources) != 1 {
		gap("Pod", "kubernetes/pods", target, strings.Join(result.Errors, "; "))
		return i
	}
	pod := result.Resources[0]
	if target.UID != "" && target.UID != pod.Ref.UID {
		gap("Pod identity changed", "kubernetes/pods", target, "The selected pod UID no longer matches; select the current pod before investigating")
		return i
	}
	i.Target = pod.Ref
	captureResource(pod, "Pod and container state")

	// Follow controller UIDs, never a name-prefix guess.
	child := pod
	for _, expected := range []struct{ kind, plural string }{{"ReplicaSet", "replicasets"}, {"Deployment", "deployments"}} {
		owners, _ := child.Details["owners"].([]any)
		var owner map[string]any
		for _, value := range owners {
			candidate, _ := value.(map[string]any)
			if candidate["controller"] == true && candidate["kind"] == expected.kind {
				owner = candidate
				break
			}
		}
		if owner == nil {
			i.Limitations = append(i.Limitations, "No supported "+expected.kind+" controller owner was captured for "+child.Ref.Name+".")
			break
		}
		name, _ := owner["name"].(string)
		uid, _ := owner["uid"].(string)
		ref := core.ResourceRef{Kind: expected.plural, Namespace: pod.Ref.Namespace, Name: name, UID: uid}
		notify("Capturing owning " + expected.kind)
		ownerResult := b.GetResource(ctx, ref.Kind, ref.Namespace, ref.Name)
		if uid == "" || len(ownerResult.Errors) > 0 || len(ownerResult.Resources) != 1 || ownerResult.Resources[0].Ref.UID != uid {
			gap("Owning "+expected.kind, "kubernetes/"+ref.Kind, ref, "Controller could not be read with the captured owner UID")
			break
		}
		child = ownerResult.Resources[0]
		captureResource(child, "Owning "+expected.kind)
	}

	notify("Capturing events for the exact pod UID")
	events := []core.Resource{}
	eventResult := b.ListResources(ctx, "events", pod.Ref.Namespace, "", 100)
	if len(eventResult.Errors) > 0 {
		gap("Pod events", "kubernetes/events", pod.Ref, strings.Join(eventResult.Errors, "; "))
	} else {
		for _, event := range eventResult.Resources {
			involved, _ := event.Details["involved_object"].(map[string]any)
			if involved["uid"] == pod.Ref.UID {
				events = append(events, event)
				i.Resources = append(i.Resources, clone(event))
			}
		}
		selector := "involvedObject.uid=" + pod.Ref.UID
		payload, _ := json.Marshal(redact.Value(map[string]any{"events": events, "matched_count": len(events), "truncated": false, "selector": selector}, 2000))
		i.AddObservation(core.Observation{Title: "Pod events", Source: "kubernetes/events", Ref: pod.Ref, Query: "LIST events namespace=" + pod.Ref.Namespace + " fieldSelector=" + selector + " limit=100", State: "sampled", Format: "json", Payload: string(payload), Notes: []string{"Synthetic fixture; events may expire and do not establish a complete history."}})
	}

	containers, _ := pod.Details["containers"].([]any)
	valid, candidates := false, []string{}
	for _, item := range containers {
		c, _ := item.(map[string]any)
		name, _ := c["name"].(string)
		valid = valid || (container != "" && name == container)
		state, _ := c["state"].(map[string]any)
		waiting, _ := state["waiting"].(map[string]any)
		if waiting["reason"] == "CrashLoopBackOff" {
			candidates = append(candidates, name)
		}
	}
	if container == "" && len(candidates) == 1 {
		container, valid = candidates[0], true
	}
	for _, previous := range []bool{true, false} {
		title := "Current container logs"
		if previous {
			title = "Previous container logs"
		}
		observation := core.Observation{Title: title, Source: "kubernetes/pods/log", Ref: pod.Ref, State: "unavailable", Format: "log", Query: fmt.Sprintf("GET pods/log namespace=%s name=%s uid=%s container=%s previous=%t tailLines=100 limitBytes=16384 sinceSeconds=900 timestamps=true", pod.Ref.Namespace, pod.Ref.Name, pod.Ref.UID, container, previous)}
		if !valid {
			observation.Errors = []string{"Choose one container with c before investigating; no log stream was guessed."}
		} else {
			notify("Capturing " + strings.ToLower(title))
			logs := b.PodLogs(ctx, pod.Ref.Namespace, pod.Ref.Name, container, previous)
			observation.Errors = logs.Errors
			if len(logs.Errors) == 0 {
				observation.State = "sampled"
				text, _ := logs.Data["logs"].(string)
				observation.Payload = redact.Text(text, 16*1024)
				observation.Notes = []string{"Synthetic bounded sample, not a complete log history."}
			}
		}
		i.AddObservation(observation)
	}
	return i
}
