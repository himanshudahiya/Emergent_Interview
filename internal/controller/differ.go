package controller

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	configv1alpha1 "github.com/example/pod-config-operator/api/v1alpha1"
)

// DiffResult describes what changed and whether eviction is required.
type DiffResult struct {
	// NeedsUpdate is true when at least one mutable field differs.
	NeedsUpdate bool
	// NeedsEviction is true when an immutable spec field (e.g. env var, resource
	// limits on some runtimes) must change; the pod must be evicted and recreated.
	NeedsEviction bool
	// Changes is a human-readable list of fields that differ, used for logging.
	Changes []string
}

// ComputeDiff compares a live pod against the desired PodTemplateOverride and
// returns a DiffResult describing what needs to change.
func ComputeDiff(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride) DiffResult {
	var result DiffResult

	diffLabels(pod, desired, &result)
	diffAnnotations(pod, desired, &result)
	diffEnv(pod, desired, &result)
	diffResources(pod, desired, &result)
	diffTolerations(pod, desired, &result)
	diffSecurityContext(pod, desired, &result)

	return result
}

// diffLabels checks whether all desired labels are present with correct values.
// Labels are mutable; no eviction needed.
func diffLabels(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride, r *DiffResult) {
	for k, want := range desired.Labels {
		if got, ok := pod.Labels[k]; !ok || got != want {
			r.NeedsUpdate = true
			r.Changes = append(r.Changes, "labels["+k+"]")
		}
	}
}

// diffAnnotations checks desired annotations. Mutable, no eviction needed.
func diffAnnotations(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride, r *DiffResult) {
	for k, want := range desired.Annotations {
		if got, ok := pod.Annotations[k]; !ok || got != want {
			r.NeedsUpdate = true
			r.Changes = append(r.Changes, "annotations["+k+"]")
		}
	}
}

// diffEnv checks whether every desired env var is present with the correct value
// in every container. Env vars are immutable on a running pod.
func diffEnv(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride, r *DiffResult) {
	if len(desired.Env) == 0 {
		return
	}

	for _, container := range pod.Spec.Containers {
		// Build a quick lookup from the container's current env.
		current := make(map[string]corev1.EnvVar, len(container.Env))
		for _, e := range container.Env {
			current[e.Name] = e
		}

		for _, want := range desired.Env {
			got, exists := current[want.Name]
			if !exists || !envVarEqual(got, want) {
				r.NeedsUpdate = true
				r.NeedsEviction = true
				r.Changes = append(r.Changes, "container["+container.Name+"].env["+want.Name+"]")
			}
		}
	}
}

// diffResources checks resource limits/requests on every container.
// Resource limits are immutable on a running pod (in most runtimes).
func diffResources(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride, r *DiffResult) {
	if desired.Resources == nil {
		return
	}

	for _, container := range pod.Spec.Containers {
		if desired.Resources.Limits != nil {
			for resource, want := range desired.Resources.Limits {
				if got, ok := container.Resources.Limits[resource]; !ok || !quantityEqual(got, want) {
					r.NeedsUpdate = true
					r.NeedsEviction = true
					r.Changes = append(r.Changes, "container["+container.Name+"].resources.limits["+resource.String()+"]")
				}
			}
		}

		if desired.Resources.Requests != nil {
			for resource, want := range desired.Resources.Requests {
				if got, ok := container.Resources.Requests[resource]; !ok || !quantityEqual(got, want) {
					r.NeedsUpdate = true
					r.NeedsEviction = true
					r.Changes = append(r.Changes, "container["+container.Name+"].resources.requests["+resource.String()+"]")
				}
			}
		}
	}
}

// diffTolerations checks whether all desired tolerations are present on the pod.
// Tolerations are immutable after creation.
func diffTolerations(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride, r *DiffResult) {
	if len(desired.Tolerations) == 0 {
		return
	}

	existing := make(map[string]corev1.Toleration)
	for _, t := range pod.Spec.Tolerations {
		existing[tolerationKey(t)] = t
	}

	for _, want := range desired.Tolerations {
		got, ok := existing[tolerationKey(want)]
		if !ok || !reflect.DeepEqual(got, want) {
			r.NeedsUpdate = true
			r.NeedsEviction = true
			r.Changes = append(r.Changes, "spec.tolerations["+tolerationKey(want)+"]")
		}
	}
}

// diffSecurityContext checks pod-level securityContext fields.
// PodSecurityContext is immutable; any change requires eviction.
func diffSecurityContext(pod *corev1.Pod, desired configv1alpha1.PodTemplateOverride, r *DiffResult) {
	if desired.SecurityContext == nil {
		return
	}

	if !reflect.DeepEqual(pod.Spec.SecurityContext, desired.SecurityContext) {
		r.NeedsUpdate = true
		r.NeedsEviction = true
		r.Changes = append(r.Changes, "spec.securityContext")
	}
}

// ----------------------- helpers -----------------------

func envVarEqual(a, b corev1.EnvVar) bool {
	if a.Name != b.Name {
		return false
	}
	// Plain value comparison.
	if a.Value != b.Value {
		return false
	}
	// ValueFrom comparison (pointer-safe deep equal).
	return reflect.DeepEqual(a.ValueFrom, b.ValueFrom)
}

func quantityEqual(a, b resource.Quantity) bool {
	return a.Cmp(b) == 0
}

// tolerationKey produces a string key unique to a toleration's key+effect pair.
func tolerationKey(t corev1.Toleration) string {
	return t.Key + "/" + string(t.Effect)
}
