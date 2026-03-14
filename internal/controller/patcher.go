package controller

import (
	corev1 "k8s.io/api/core/v1"

	configv1alpha1 "github.com/example/pod-config-operator/api/v1alpha1"
)

// ApplyMutableFields merges all mutable desired fields (labels, annotations)
// onto the pod in-place. Returns true if anything was changed.
func ApplyMutableFields(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride) bool {
	changed := false

	// Merge labels.
	if len(desired.Labels) > 0 {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		for k, v := range desired.Labels {
			if pod.Labels[k] != v {
				pod.Labels[k] = v
				changed = true
			}
		}
	}

	// Merge annotations.
	if len(desired.Annotations) > 0 {
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		for k, v := range desired.Annotations {
			if pod.Annotations[k] != v {
				pod.Annotations[k] = v
				changed = true
			}
		}
	}

	return changed
}

// BuildRecreatedPod returns a new Pod object with the desired immutable fields
// applied on top of the existing pod's spec. The new pod:
//   - Strips fields that the API server manages (resourceVersion, uid,
//     creationTimestamp, status, ownerReferences) so it can be re-submitted.
//   - Preserves everything else from the original pod.
func BuildRecreatedPod(existing *corev1.Pod, desired configv1alpha1.PodTemplateOverride) *corev1.Pod {
	// Deep-copy so we never mutate the cached object.
	pod := existing.DeepCopy()

	// Strip API-server-managed / immutable identity fields.
	pod.ResourceVersion = ""
	pod.UID = ""
	pod.CreationTimestamp = metav1Zero()
	pod.DeletionTimestamp = nil
	pod.DeletionGracePeriodSeconds = nil
	pod.Status = corev1.PodStatus{}
	pod.Finalizers = nil
	// Node name is scheduler's job; clear it so the new pod gets rescheduled.
	pod.Spec.NodeName = ""

	// Apply desired mutable metadata fields.
	ApplyMutableFields(pod, desired)

	// Apply env vars to every container.
	applyEnvToContainers(pod.Spec.Containers, desired.Env)
	applyEnvToContainers(pod.Spec.InitContainers, desired.Env)

	// Apply resource requirements to every container.
	if desired.Resources != nil {
		applyResourcesToContainers(pod.Spec.Containers, desired.Resources)
		applyResourcesToContainers(pod.Spec.InitContainers, desired.Resources)
	}

	// Merge tolerations.
	if len(desired.Tolerations) > 0 {
		pod.Spec.Tolerations = mergeTolerations(pod.Spec.Tolerations, desired.Tolerations)
	}

	// Apply security context.
	if desired.SecurityContext != nil {
		pod.Spec.SecurityContext = desired.SecurityContext.DeepCopy()
	}

	return pod
}

// applyEnvToContainers merges desired env vars into a slice of containers.
// An existing var with the same name is overwritten; novel vars are appended.
func applyEnvToContainers(containers []corev1.Container, envVars []corev1.EnvVar) {
	for i := range containers {
		containers[i].Env = mergeEnv(containers[i].Env, envVars)
	}
}

// mergeEnv merges desired env vars into existing, overwriting on name collision.
func mergeEnv(existing, desired []corev1.EnvVar) []corev1.EnvVar {
	byName := make(map[string]int, len(existing))
	for i, e := range existing {
		byName[e.Name] = i
	}

	result := make([]corev1.EnvVar, len(existing))
	copy(result, existing)

	for _, want := range desired {
		if idx, found := byName[want.Name]; found {
			result[idx] = want // overwrite
		} else {
			result = append(result, want)
			byName[want.Name] = len(result) - 1
		}
	}

	return result
}

// applyResourcesToContainers sets resource limits/requests on each container.
func applyResourcesToContainers(containers []corev1.Container, res *corev1.ResourceRequirements) {
	for i := range containers {
		if res.Limits != nil {
			if containers[i].Resources.Limits == nil {
				containers[i].Resources.Limits = corev1.ResourceList{}
			}
			for k, v := range res.Limits {
				containers[i].Resources.Limits[k] = v
			}
		}
		if res.Requests != nil {
			if containers[i].Resources.Requests == nil {
				containers[i].Resources.Requests = corev1.ResourceList{}
			}
			for k, v := range res.Requests {
				containers[i].Resources.Requests[k] = v
			}
		}
	}
}

// mergeTolerations merges desired tolerations into existing ones.
// Tolerations with the same key+effect are replaced; novel ones are appended.
func mergeTolerations(existing, desired []corev1.Toleration) []corev1.Toleration {
	byKey := make(map[string]int, len(existing))
	for i, t := range existing {
		byKey[tolerationKey(t)] = i
	}

	result := make([]corev1.Toleration, len(existing))
	copy(result, existing)

	for _, want := range desired {
		if idx, found := byKey[tolerationKey(want)]; found {
			result[idx] = want
		} else {
			result = append(result, want)
		}
	}

	return result
}

// metav1Zero returns a zero-value metav1.Time (used to clear timestamps).
func metav1Zero() metav1.Time {
	return metav1.Time{}
}
