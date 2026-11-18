package kube

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubix/internal/core"
	"kubix/internal/redact"
)

func timestamp(value metav1.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.Time.Format(time.RFC3339)
}

func resourceRef(kind string, meta metav1.ObjectMeta) core.ResourceRef {
	return core.ResourceRef{Kind: kind, Namespace: meta.Namespace, Name: meta.Name, UID: string(meta.UID)}
}

func finish(ref core.ResourceRef, status, summary string, details map[string]any) core.Resource {
	return core.Resource{Ref: ref, Status: redact.Text(status, 100), Summary: redact.Text(summary, 300),
		Details: redact.Value(details, 2000).(map[string]any)}
}

func containerState(state corev1.ContainerState) map[string]any {
	result := map[string]any{}
	if value := state.Waiting; value != nil {
		result["waiting"] = map[string]any{"reason": value.Reason, "message": value.Message}
	}
	if value := state.Running; value != nil {
		result["running"] = map[string]any{"started_at": timestamp(value.StartedAt)}
	}
	if value := state.Terminated; value != nil {
		result["terminated"] = map[string]any{"reason": value.Reason, "message": value.Message,
			"exit_code": value.ExitCode, "signal": value.Signal,
			"started_at": timestamp(value.StartedAt), "finished_at": timestamp(value.FinishedAt)}
	}
	return result
}

func resourceQuantities(list corev1.ResourceList) map[string]any {
	result := map[string]any{}
	for name, value := range list {
		result[string(name)] = value.String()
	}
	return result
}

func containerDetails(spec corev1.PodSpec, status corev1.PodStatus) []any {
	result := []any{}
	groups := []struct {
		kind     string
		specs    []corev1.Container
		statuses []corev1.ContainerStatus
	}{
		{"regular", spec.Containers, status.ContainerStatuses},
		{"init", spec.InitContainers, status.InitContainerStatuses},
	}
	for _, group := range groups {
		states := map[string]corev1.ContainerStatus{}
		for _, item := range group.statuses {
			states[item.Name] = item
		}
		for _, item := range group.specs[:min(len(group.specs), 20)] {
			state := states[item.Name]
			ports := []any{}
			for _, port := range item.Ports[:min(len(item.Ports), 20)] {
				protocol := port.Protocol
				if protocol == "" {
					protocol = corev1.ProtocolTCP
				}
				ports = append(ports, map[string]any{"name": port.Name, "container_port": port.ContainerPort, "protocol": protocol})
			}
			result = append(result, map[string]any{
				"name": item.Name, "kind": group.kind, "image": item.Image,
				"ports":              ports,
				"resources":          map[string]any{"requests": resourceQuantities(item.Resources.Requests), "limits": resourceQuantities(item.Resources.Limits)},
				"has_liveness_probe": item.LivenessProbe != nil, "has_readiness_probe": item.ReadinessProbe != nil,
				"has_startup_probe": item.StartupProbe != nil, "ready": state.Ready, "restart_count": state.RestartCount,
				"state": containerState(state.State), "last_state": containerState(state.LastTerminationState),
			})
		}
	}
	return result[:min(len(result), 30)]
}

func podResource(pod *corev1.Pod) core.Resource {
	status := string(pod.Status.Phase)
	if status == "" {
		status = "Unknown"
	}
	if pod.Status.Reason != "" {
		status = pod.Status.Reason
	}
	states := append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...)
	waiting, crash, oom, lastOOM, restarts := "", false, false, false, int32(0)
	for _, item := range states {
		restarts += item.RestartCount
		if item.State.Waiting != nil {
			if waiting == "" {
				waiting = item.State.Waiting.Reason
			}
			crash = crash || item.State.Waiting.Reason == "CrashLoopBackOff"
		}
		oom = oom || (item.State.Terminated != nil && item.State.Terminated.Reason == "OOMKilled")
		lastOOM = lastOOM || (item.LastTerminationState.Terminated != nil && item.LastTerminationState.Terminated.Reason == "OOMKilled")
	}
	if crash {
		status = "CrashLoopBackOff"
	} else if waiting != "" {
		status = waiting
	} else if oom {
		status = "OOMKilled"
	}
	ready := 0
	for _, item := range pod.Status.ContainerStatuses {
		if item.Ready {
			ready++
		}
	}
	node := pod.Spec.NodeName
	if node == "" {
		node = "-"
	}
	summary := fmt.Sprintf("Ready %d/%d | restarts %d | node %s", ready, len(pod.Spec.Containers), restarts, node)
	if lastOOM {
		summary += " | last exit OOMKilled"
	}
	conditions := []any{}
	for _, item := range pod.Status.Conditions[:min(len(pod.Status.Conditions), 20)] {
		conditions = append(conditions, map[string]any{"type": string(item.Type), "status": string(item.Status),
			"reason": item.Reason, "message": item.Message, "last_transition_time": timestamp(item.LastTransitionTime)})
	}
	owners := []any{}
	for _, item := range pod.OwnerReferences[:min(len(pod.OwnerReferences), 10)] {
		owners = append(owners, map[string]any{"kind": item.Kind, "name": item.Name, "uid": string(item.UID),
			"controller": item.Controller != nil && *item.Controller})
	}
	labels, labelsTruncated := podLabels(pod.Labels)
	return finish(resourceRef("pods", pod.ObjectMeta), status, summary, map[string]any{
		"created_at": timestamp(pod.CreationTimestamp), "generation": pod.Generation,
		"phase": string(pod.Status.Phase), "reason": pod.Status.Reason, "message": pod.Status.Message,
		"node_name": pod.Spec.NodeName, "restart_policy": string(pod.Spec.RestartPolicy),
		"labels": labels, "labels_truncated": labelsTruncated,
		"containers": containerDetails(pod.Spec, pod.Status), "conditions": conditions, "owners": owners,
	})
}

const maxPodLabels = 50

func podLabels(input map[string]string) (map[string]string, bool) {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	truncated := len(keys) > maxPodLabels
	keys = keys[:min(len(keys), maxPodLabels)]
	labels := make(map[string]string, len(keys))
	for _, key := range keys {
		labels[key] = input[key]
	}
	return labels, truncated
}

func deploymentResource(deployment *appsv1.Deployment) core.Resource {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	status := "Progressing"
	if deployment.Status.AvailableReplicas >= desired && deployment.Status.ObservedGeneration >= deployment.Generation {
		status = "Available"
	}
	if desired == 0 {
		status = "ScaledToZero"
	}
	conditions := []any{}
	for _, item := range deployment.Status.Conditions[:min(len(deployment.Status.Conditions), 20)] {
		if desired > 0 && item.Type == appsv1.DeploymentProgressing && item.Status == corev1.ConditionFalse {
			status = "Degraded"
		}
		conditions = append(conditions, map[string]any{"type": string(item.Type), "status": string(item.Status),
			"reason": item.Reason, "message": item.Message, "last_transition_time": timestamp(item.LastTransitionTime)})
	}
	return finish(resourceRef("deployments", deployment.ObjectMeta), status,
		fmt.Sprintf("Ready %d/%d | updated %d", deployment.Status.ReadyReplicas, desired, deployment.Status.UpdatedReplicas),
		map[string]any{"created_at": timestamp(deployment.CreationTimestamp), "generation": deployment.Generation,
			"desired_replicas": desired, "ready_replicas": deployment.Status.ReadyReplicas,
			"available_replicas": deployment.Status.AvailableReplicas, "updated_replicas": deployment.Status.UpdatedReplicas,
			"observed_generation": deployment.Status.ObservedGeneration, "conditions": conditions,
			"containers": containerDetails(deployment.Spec.Template.Spec, corev1.PodStatus{}),
		})
}

// ReplicaSets are exposed for browsing a verified Pod owner relationship.
// Keep the representation as narrow as the other resource details: no env,
// commands, arbitrary annotations, or configuration values.
func replicaSetResource(replica *appsv1.ReplicaSet) core.Resource {
	desired := int32(1)
	if replica.Spec.Replicas != nil {
		desired = *replica.Spec.Replicas
	}
	status := "Progressing"
	if replica.Status.ReadyReplicas >= desired && replica.Status.ObservedGeneration >= replica.Generation {
		status = "Ready"
	}
	if desired == 0 {
		status = "ScaledToZero"
	}
	owners := []any{}
	for _, owner := range replica.OwnerReferences[:min(len(replica.OwnerReferences), 10)] {
		owners = append(owners, map[string]any{"kind": owner.Kind, "name": owner.Name, "uid": string(owner.UID),
			"controller": owner.Controller != nil && *owner.Controller})
	}
	return finish(resourceRef("replicasets", replica.ObjectMeta), status,
		fmt.Sprintf("Ready %d/%d", replica.Status.ReadyReplicas, desired), map[string]any{
			"created_at": timestamp(replica.CreationTimestamp), "generation": replica.Generation,
			"observed_generation": replica.Status.ObservedGeneration,
			"desired_replicas":    desired, "ready_replicas": replica.Status.ReadyReplicas,
			"available_replicas": replica.Status.AvailableReplicas,
			"revision":           replica.Annotations["deployment.kubernetes.io/revision"],
			"owners":             owners, "containers": containerDetails(replica.Spec.Template.Spec, corev1.PodStatus{}),
		})
}

func serviceResource(service *corev1.Service) core.Resource {
	ports, portNames := []any{}, []string{}
	for _, item := range service.Spec.Ports[:min(len(service.Spec.Ports), 20)] {
		protocol := string(item.Protocol)
		if protocol == "" {
			protocol = "TCP"
		}
		ports = append(ports, map[string]any{"name": item.Name, "port": item.Port, "target_port": item.TargetPort.String(),
			"protocol": protocol, "node_port": item.NodePort})
		portNames = append(portNames, fmt.Sprintf("%d/%s", item.Port, protocol))
	}
	selector := map[string]any{}
	for key, value := range service.Spec.Selector {
		selector[key] = value
	}
	status := string(service.Spec.Type)
	if status == "" {
		status = "ClusterIP"
	}
	return finish(resourceRef("services", service.ObjectMeta), status,
		service.Spec.ClusterIP+" | "+strings.Join(portNames, ", "),
		map[string]any{"created_at": timestamp(service.CreationTimestamp), "cluster_ip": service.Spec.ClusterIP,
			"ports": ports, "selector": selector, "external_name": service.Spec.ExternalName})
}

func endpointSliceResource(slice *discoveryv1.EndpointSlice) core.Resource {
	const maxEndpoints = 30
	ports := []any{}
	portNames := []string{}
	for _, item := range slice.Ports[:min(len(slice.Ports), 20)] {
		protocol := corev1.ProtocolTCP
		if item.Protocol != nil && *item.Protocol != "" {
			protocol = *item.Protocol
		}
		var port any
		if item.Port != nil {
			port = *item.Port
			portNames = append(portNames, fmt.Sprintf("%d/%s", *item.Port, protocol))
		}
		var name, appProtocol any
		if item.Name != nil {
			name = *item.Name
		}
		if item.AppProtocol != nil {
			appProtocol = *item.AppProtocol
		}
		ports = append(ports, map[string]any{"name": name, "port": port, "protocol": protocol, "app_protocol": appProtocol})
	}

	readyCount, readyKnownCount := 0, 0
	endpoints := []any{}
	for _, item := range slice.Endpoints[:min(len(slice.Endpoints), maxEndpoints)] {
		conditions := map[string]any{}
		if item.Conditions.Ready != nil {
			readyKnownCount++
			conditions["ready"] = *item.Conditions.Ready
			if *item.Conditions.Ready {
				readyCount++
			}
		}
		if item.Conditions.Serving != nil {
			conditions["serving"] = *item.Conditions.Serving
		}
		if item.Conditions.Terminating != nil {
			conditions["terminating"] = *item.Conditions.Terminating
		}
		var targetRef any
		if item.TargetRef != nil {
			targetRef = map[string]any{"kind": item.TargetRef.Kind, "name": item.TargetRef.Name,
				"namespace": item.TargetRef.Namespace, "uid": string(item.TargetRef.UID)}
		}
		endpoints = append(endpoints, map[string]any{"address_count": min(len(item.Addresses), 20),
			"conditions": conditions, "target_ref": targetRef})
	}
	serviceName := slice.Labels[discoveryv1.LabelServiceName]
	if serviceName == "" {
		serviceName = "unknown"
	}
	observedEndpoints := min(len(slice.Endpoints), maxEndpoints)
	summary := fmt.Sprintf("Service %s | endpoints %d | ready %d/%d known in sample of %d | ports %s", serviceName,
		len(slice.Endpoints), readyCount, readyKnownCount, observedEndpoints, strings.Join(portNames, ", "))
	return finish(resourceRef("endpointslices", slice.ObjectMeta), "Observed", summary, map[string]any{
		"service_name": slice.Labels[discoveryv1.LabelServiceName], "address_type": slice.AddressType,
		"ports": ports, "endpoints": endpoints, "endpoint_count": len(slice.Endpoints),
		"ready_count_sampled": readyCount, "ready_condition_count_sampled": readyKnownCount,
		"sampled_endpoint_count": observedEndpoints,
		"endpoints_truncated":    len(slice.Endpoints) > maxEndpoints,
		"created_at":             timestamp(slice.CreationTimestamp),
	})
}

func eventResource(event *corev1.Event) core.Resource {
	status := event.Type
	if status == "" {
		status = "Unknown"
	}
	return finish(resourceRef("events", event.ObjectMeta), status, event.Reason+": "+event.Message,
		map[string]any{"created_at": timestamp(event.CreationTimestamp), "reason": event.Reason, "message": event.Message,
			"count": event.Count, "first_timestamp": timestamp(event.FirstTimestamp), "last_timestamp": timestamp(event.LastTimestamp),
			"event_time": timestamp(metav1.NewTime(event.EventTime.Time)),
			"involved_object": map[string]any{"kind": event.InvolvedObject.Kind, "name": event.InvolvedObject.Name,
				"namespace": event.InvolvedObject.Namespace, "uid": string(event.InvolvedObject.UID)}})
}

func nodeResource(node *corev1.Node) core.Resource {
	status, conditions := "NotReady", []any{}
	for _, item := range node.Status.Conditions[:min(len(node.Status.Conditions), 20)] {
		if item.Type == corev1.NodeReady && item.Status == corev1.ConditionTrue {
			status = "Ready"
		}
		conditions = append(conditions, map[string]any{"type": string(item.Type), "status": string(item.Status),
			"reason": item.Reason, "message": item.Message, "last_transition_time": timestamp(item.LastTransitionTime)})
	}
	capacity, allocatable := map[string]any{}, map[string]any{}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourcePods} {
		if value, ok := node.Status.Capacity[name]; ok {
			capacity[string(name)] = value.String()
		}
		if value, ok := node.Status.Allocatable[name]; ok {
			allocatable[string(name)] = value.String()
		}
	}
	summary := fmt.Sprintf("CPU %v | memory %v", capacity["cpu"], capacity["memory"])
	if node.Spec.Unschedulable {
		summary += " | scheduling disabled"
	}
	taints := []any{}
	for _, item := range node.Spec.Taints[:min(len(node.Spec.Taints), 20)] {
		taints = append(taints, map[string]any{"key": item.Key, "value": item.Value, "effect": string(item.Effect)})
	}
	ref := resourceRef("nodes", node.ObjectMeta)
	ref.Namespace = ""
	return finish(ref, status, summary, map[string]any{"created_at": timestamp(node.CreationTimestamp),
		"conditions": conditions, "unschedulable": node.Spec.Unschedulable, "capacity": capacity,
		"allocatable": allocatable, "taints": taints, "kubelet_version": node.Status.NodeInfo.KubeletVersion})
}
