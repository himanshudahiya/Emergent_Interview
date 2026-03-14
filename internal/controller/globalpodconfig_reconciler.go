package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	configv1alpha1 "github.com/example/pod-config-operator/api/v1alpha1"
)

// GlobalPodConfigReconciler reconciles a GlobalPodConfig object.
// +kubebuilder:rbac:groups=config.example.com,resources=globalpodconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=config.example.com,resources=globalpodconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
type GlobalPodConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the manager.
func (r *GlobalPodConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configv1alpha1.GlobalPodConfig{}).
		// Also re-enqueue when any pod changes (for drift detection).
		Watches(&corev1.Pod{}, &podEventHandler{}).
		Complete(r)
}

// Reconcile is called whenever a GlobalPodConfig or a watched Pod changes,
// and also periodically via RequeueAfter.
func (r *GlobalPodConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("GlobalPodConfig", req.Name)
	logger.Info("Starting reconcile")

	// ------------------------------------------------------------------ 1.
	// Fetch the GlobalPodConfig CR (cluster-scoped, no namespace in req).
	// ------------------------------------------------------------------ 1.
	gpc := &configv1alpha1.GlobalPodConfig{}
	if err := r.Get(ctx, req.NamespacedName, gpc); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted — nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GlobalPodConfig: %w", err)
	}

	// Mark as Syncing while we work.
	if err := r.setCondition(ctx, gpc, configv1alpha1.ConditionSyncing, corev1.ConditionTrue, "ReconcileStarted", ""); err != nil {
		logger.Error(err, "Failed to set Syncing condition")
	}

	// ------------------------------------------------------------------ 2.
	// Parse the sync interval from the spec.
	// ------------------------------------------------------------------ 2.
	syncInterval, err := time.ParseDuration(gpc.Spec.SyncInterval)
	if err != nil || syncInterval <= 0 {
		syncInterval = 30 * time.Second
	}

	// ------------------------------------------------------------------ 3.
	// Build a label selector from the CR spec.
	// ------------------------------------------------------------------ 3.
	selector, err := metav1.LabelSelectorAsSelector(&gpc.Spec.LabelSelector)
	if err != nil {
		return ctrl.Result{}, r.failWithCondition(ctx, gpc, "InvalidSelector",
			fmt.Sprintf("cannot parse labelSelector: %v", err))
	}

	// ------------------------------------------------------------------ 4.
	// List all pods across every namespace that match the selector.
	// ------------------------------------------------------------------ 4.
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		&client.ListOptions{LabelSelector: selector},
		// No namespace restriction — cluster-wide scan.
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing pods: %w", err)
	}

	logger.Info("Pods matched", "count", len(podList.Items))

	// ------------------------------------------------------------------ 5.
	// Reconcile each pod concurrently, bounded by MaxConcurrent.
	// ------------------------------------------------------------------ 5.
	stats, err := r.reconcilePods(ctx, logger, gpc, podList.Items)
	if err != nil {
		return ctrl.Result{}, r.failWithCondition(ctx, gpc, "ReconcileError", err.Error())
	}

	// ------------------------------------------------------------------ 6.
	// Write status back onto the CR.
	// ------------------------------------------------------------------ 6.
	now := metav1.Now()
	gpc.Status.MatchedPods = int32(len(podList.Items))
	gpc.Status.UpdatedPods = stats.updated
	gpc.Status.SkippedPods = stats.skipped
	gpc.Status.LastSyncTime = &now
	gpc.Status.ObservedGeneration = gpc.Generation

	if err := r.setCondition(ctx, gpc, configv1alpha1.ConditionReady, corev1.ConditionTrue,
		"ReconcileComplete",
		fmt.Sprintf("matched=%d updated=%d skipped=%d", len(podList.Items), stats.updated, stats.skipped),
	); err != nil {
		logger.Error(err, "Failed writing Ready condition")
	}

	logger.Info("Reconcile complete", "matched", len(podList.Items), "updated", stats.updated, "skipped", stats.skipped)

	// Schedule the next full scan regardless of watch events.
	return ctrl.Result{RequeueAfter: syncInterval}, nil
}

// ------------------------------------------------------------------ pod loop

type reconcileStats struct {
	updated int32
	skipped int32
}

// reconcilePods fans out pod reconciliation across a semaphore-bounded worker pool.
func (r *GlobalPodConfigReconciler) reconcilePods(
	ctx context.Context,
	logger logr.Logger,
	gpc *configv1alpha1.GlobalPodConfig,
	pods []corev1.Pod,
) (reconcileStats, error) {
	maxConcurrent := int(gpc.Spec.EvictionPolicy.MaxConcurrent)
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}

	sem := make(chan struct{}, maxConcurrent)

	var (
		mu      sync.Mutex
		stats   reconcileStats
		firstErr error
		wg      sync.WaitGroup
	)

	for i := range pods {
		pod := pods[i] // capture loop variable

		// Skip pods that are already terminating.
		if pod.DeletionTimestamp != nil {
			continue
		}

		wg.Add(1)
		sem <- struct{}{} // acquire slot

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // release slot

			updated, skipped, err := r.reconcileSinglePod(ctx, logger, gpc, &pod)

			mu.Lock()
			defer mu.Unlock()

			if err != nil && firstErr == nil {
				firstErr = err
			}
			if updated {
				stats.updated++
			}
			if skipped {
				stats.skipped++
			}
		}()
	}

	wg.Wait()
	return stats, firstErr
}

// reconcileSinglePod diffs one pod against the desired spec and acts on the result.
// Returns (updated, skipped, error).
func (r *GlobalPodConfigReconciler) reconcileSinglePod(
	ctx context.Context,
	logger logr.Logger,
	gpc *configv1alpha1.GlobalPodConfig,
	pod *corev1.Pod,
) (updated, skipped bool, err error) {
	podKey := pod.Namespace + "/" + pod.Name
	desired := gpc.Spec.PodTemplate
	evictionPolicy := gpc.Spec.EvictionPolicy

	diff := ComputeDiff(pod, desired)

	if !diff.NeedsUpdate {
		logger.V(1).Info("Pod is in sync", "pod", podKey)
		return false, false, nil
	}

	logger.Info("Pod has drift", "pod", podKey, "changes", diff.Changes)

	// ---- Case A: only mutable fields differ → PATCH metadata in-place ----
	if !diff.NeedsEviction {
		patchBase := pod.DeepCopy()
		ApplyMutableFields(patchBase, desired)

		if err := r.Patch(ctx, patchBase, client.MergeFrom(pod)); err != nil {
			return false, false, fmt.Errorf("patching pod %s: %w", podKey, err)
		}
		logger.Info("Patched pod (mutable fields)", "pod", podKey)
		return true, false, nil
	}

	// ---- Case B: immutable field differs → evict / delete + recreate ----
	strategy := evictionPolicy.Strategy
	if strategy == "" {
		strategy = configv1alpha1.EvictionStrategyEvict
	}

	switch strategy {
	case configv1alpha1.EvictionStrategySkip:
		logger.Info("Skipping pod (eviction disabled by policy)", "pod", podKey, "changes", diff.Changes)
		return false, true, nil

	case configv1alpha1.EvictionStrategyDelete:
		if err := r.forceDeletePod(ctx, pod, evictionPolicy.GracePeriodSeconds); err != nil {
			return false, false, fmt.Errorf("force-deleting pod %s: %w", podKey, err)
		}

	case configv1alpha1.EvictionStrategyEvict:
		if err := r.evictPod(ctx, pod, evictionPolicy.GracePeriodSeconds); err != nil {
			return false, false, fmt.Errorf("evicting pod %s: %w", podKey, err)
		}
	}

	// ---- Recreate the pod with the desired spec applied ----
	newPod := BuildRecreatedPod(pod, desired)
	if err := r.Create(ctx, newPod); err != nil {
		// If the pod already exists (race), treat it as non-fatal — the next
		// reconcile will pick it up.
		if errors.IsAlreadyExists(err) {
			logger.Info("Pod already recreated (race)", "pod", podKey)
			return true, false, nil
		}
		return false, false, fmt.Errorf("recreating pod %s: %w", podKey, err)
	}

	logger.Info("Evicted and recreated pod", "pod", podKey, "strategy", strategy)
	return true, false, nil
}

// ------------------------------------------------------------------ eviction helpers

// evictPod uses the Eviction sub-resource, which respects PodDisruptionBudgets.
func (r *GlobalPodConfigReconciler) evictPod(ctx context.Context, pod *corev1.Pod, gracePeriod int64) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		},
	}
	return r.Client.SubResource("eviction").Create(ctx, pod, eviction)
}

// forceDeletePod issues a raw Delete, bypassing PDB checks.
func (r *GlobalPodConfigReconciler) forceDeletePod(ctx context.Context, pod *corev1.Pod, gracePeriod int64) error {
	return r.Delete(ctx, pod, &client.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
}

// ------------------------------------------------------------------ status helpers

// setCondition upserts a condition on the CR and patches the status sub-resource.
func (r *GlobalPodConfigReconciler) setCondition(
	ctx context.Context,
	gpc *configv1alpha1.GlobalPodConfig,
	condType configv1alpha1.ConditionType,
	status corev1.ConditionStatus,
	reason, message string,
) error {
	now := metav1.Now()
	newCond := configv1alpha1.GlobalPodConfigCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}

	// Upsert into the conditions slice.
	found := false
	for i, c := range gpc.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				gpc.Status.Conditions[i] = newCond
			}
			found = true
			break
		}
	}
	if !found {
		gpc.Status.Conditions = append(gpc.Status.Conditions, newCond)
	}

	return r.Status().Update(ctx, gpc)
}

// failWithCondition sets Degraded=True and returns a wrapped error.
func (r *GlobalPodConfigReconciler) failWithCondition(
	ctx context.Context,
	gpc *configv1alpha1.GlobalPodConfig,
	reason, message string,
) error {
	_ = r.setCondition(ctx, gpc, configv1alpha1.ConditionDegraded, corev1.ConditionTrue, reason, message)
	return fmt.Errorf("%s: %s", reason, message)
}

// ------------------------------------------------------------------ pod event handler

// podEventHandler enqueues the GlobalPodConfig name whenever any pod changes.
// Because GlobalPodConfig is cluster-scoped there may be multiple CRs; we
// enqueue all of them so the reconciler can re-check each selector.
type podEventHandler struct{}

func (h *podEventHandler) Create(ctx context.Context, evt event.CreateEvent, q workqueue.RateLimitingInterface) {
	h.enqueueAll(ctx, q)
}
func (h *podEventHandler) Update(ctx context.Context, evt event.UpdateEvent, q workqueue.RateLimitingInterface) {
	h.enqueueAll(ctx, q)
}
func (h *podEventHandler) Delete(ctx context.Context, evt event.DeleteEvent, q workqueue.RateLimitingInterface) {
}
func (h *podEventHandler) Generic(ctx context.Context, evt event.GenericEvent, q workqueue.RateLimitingInterface) {
}

// enqueueAll is a placeholder; in practice you'd inject a client to list
// all GlobalPodConfig CRs and enqueue each one.
func (h *podEventHandler) enqueueAll(ctx context.Context, q workqueue.RateLimitingInterface) {
	// TODO: inject client, list all GlobalPodConfig CRs, enqueue each.
	// For most use-cases a single CR is sufficient and you can hardcode
	// the name here, or disable pod watches and rely on RequeueAfter only.
	_ = ctx
	_ = q
}

// Ensure the reconciler implements the Reconciler interface.
var _ = &GlobalPodConfigReconciler{}
