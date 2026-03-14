// Package v1alpha1 contains API Schema definitions for the config v1alpha1 API group.
// +groupName=config.example.com
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ----------------------- Spec types -----------------------

// GlobalPodConfigSpec defines the desired state of GlobalPodConfig.
type GlobalPodConfigSpec struct {
	// LabelSelector selects pods across all namespaces to be managed.
	// +kubebuilder:validation:Required
	LabelSelector metav1.LabelSelector `json:"labelSelector"`

	// PodTemplate describes the desired state to enforce on matched pods.
	// Only fields present here are reconciled; everything else is left untouched.
	// +kubebuilder:validation:Required
	PodTemplate PodTemplateOverride `json:"podTemplate"`

	// EvictionPolicy controls behaviour when a pod has immutable spec fields
	// that must change (e.g. container image, resource limits on some runtimes).
	// +optional
	EvictionPolicy EvictionPolicy `json:"evictionPolicy,omitempty"`

	// SyncInterval is how often the full reconcile loop runs regardless of
	// watch events. Parsed as a Go duration string (e.g. "30s", "5m").
	// +kubebuilder:default="30s"
	// +optional
	SyncInterval string `json:"syncInterval,omitempty"`
}

// PodTemplateOverride holds the fields the operator will enforce on matched pods.
// Every field is optional; only present fields are reconciled.
type PodTemplateOverride struct {
	// Labels to merge-patch onto pod metadata.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to merge-patch onto pod metadata.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Env vars to inject into every container in the pod.
	// Vars with the same name as existing ones are overwritten.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources to enforce on every container. Applied as a merge-patch so
	// existing limits/requests not mentioned here are left alone.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Tolerations to merge onto the pod spec. Duplicates (same key+effect) are
	// replaced; novel tolerations are appended.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// SecurityContext to enforce at the pod level.
	// +optional
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
}

// EvictionStrategy describes what the operator does when it must change an
// immutable pod spec field.
// +kubebuilder:validation:Enum=Evict;Delete;Skip
type EvictionStrategy string

const (
	// EvictionStrategyEvict uses the Eviction sub-resource (respects PDBs).
	EvictionStrategyEvict EvictionStrategy = "Evict"
	// EvictionStrategyDelete force-deletes the pod.
	EvictionStrategyDelete EvictionStrategy = "Delete"
	// EvictionStrategySkip logs the conflict and leaves the pod untouched.
	EvictionStrategySkip EvictionStrategy = "Skip"
)

// EvictionPolicy controls how the operator recreates pods with immutable fields.
type EvictionPolicy struct {
	// Strategy is the eviction method. Defaults to Evict.
	// +kubebuilder:default=Evict
	// +optional
	Strategy EvictionStrategy `json:"strategy,omitempty"`

	// MaxConcurrent is the maximum number of pods to evict in parallel.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`

	// GracePeriodSeconds is the termination grace period forwarded to the
	// eviction/delete call. Defaults to 30.
	// +kubebuilder:default=30
	// +optional
	GracePeriodSeconds int64 `json:"gracePeriodSeconds,omitempty"`
}

// ----------------------- Status types -----------------------

// ConditionType enumerates the condition types reported on a GlobalPodConfig.
// +kubebuilder:validation:Enum=Ready;Syncing;Degraded
type ConditionType string

const (
	ConditionReady    ConditionType = "Ready"
	ConditionSyncing  ConditionType = "Syncing"
	ConditionDegraded ConditionType = "Degraded"
)

// GlobalPodConfigCondition is a single condition on the CR.
type GlobalPodConfigCondition struct {
	Type   ConditionType          `json:"type"`
	Status corev1.ConditionStatus `json:"status"`

	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// GlobalPodConfigStatus is written back by the operator after each reconcile.
type GlobalPodConfigStatus struct {
	// MatchedPods is the total number of pods found by the selector in the last sync.
	MatchedPods int32 `json:"matchedPods,omitempty"`

	// UpdatedPods is the number of pods patched or evicted in the last sync cycle.
	UpdatedPods int32 `json:"updatedPods,omitempty"`

	// SkippedPods is the number of pods skipped because evictionPolicy=Skip.
	SkippedPods int32 `json:"skippedPods,omitempty"`

	// LastSyncTime is when the last full reconcile loop completed.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// ObservedGeneration is the .metadata.generation of the CR when this
	// status was written, used for detecting stale status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions is a list of conditions that describe the current state.
	// +optional
	Conditions []GlobalPodConfigCondition `json:"conditions,omitempty"`
}

// ----------------------- Root type -----------------------

// GlobalPodConfig is the Schema for the globalpodconfigs API.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=gpc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=".status.matchedPods"
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=".status.updatedPods"
// +kubebuilder:printcolumn:name="LastSync",type=string,JSONPath=".status.lastSyncTime"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type GlobalPodConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GlobalPodConfigSpec   `json:"spec,omitempty"`
	Status GlobalPodConfigStatus `json:"status,omitempty"`
}

// GlobalPodConfigList contains a list of GlobalPodConfig.
// +kubebuilder:object:root=true
type GlobalPodConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GlobalPodConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GlobalPodConfig{}, &GlobalPodConfigList{})
}
