package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

const (
	DefaultSchedulingGate = sequence.DefaultSchedulingGate
	FinalizerKey          = "podsequence.example.com/finalizer"
	// RequeueDelay is the polling interval for cluster-scoped sequences.
	RequeueDelay = 5 * time.Second
	// NodeResync is the safety resync for node-scoped sequences (watches drive normal progress).
	NodeResync = time.Minute
	// OrphanCleanupInterval is how often stale node labels/taints are removed.
	OrphanCleanupInterval = 5 * time.Minute
)

// PodSequenceReconciler reconciles a PodSequence object
type PodSequenceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// NodeScopeEnabled enables node-scoped sequences. It requires node and DaemonSet RBAC
	// (full mode); lite mode only supports cluster scope.
	NodeScopeEnabled bool
	// Now is overridable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=scheduling.example.com,resources=podsequences,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scheduling.example.com,resources=podsequences/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scheduling.example.com,resources=podsequences/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *PodSequenceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *PodSequenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	seq := &schedulingv1alpha1.PodSequence{}
	if err := r.Get(ctx, req.NamespacedName, seq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !seq.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, seq)
	}

	if !controllerutil.ContainsFinalizer(seq, FinalizerKey) {
		controllerutil.AddFinalizer(seq, FinalizerKey)
		return ctrl.Result{}, r.Update(ctx, seq)
	}

	orig := seq.DeepCopy()
	scope := sequence.Scope(seq)

	if scope == schedulingv1alpha1.PodSequenceScopeNode && !r.NodeScopeEnabled {
		seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseFailed
		seq.Status.Message = "Node-scoped sequencing requires full mode. Use scope: Cluster or redeploy with --enable-webhook flag."
		if orig.Status.Message != seq.Status.Message {
			r.Recorder.Event(seq, corev1.EventTypeWarning, "WebhookRequired", "Node-scoped sequencing requires full mode (--enable-webhook)")
		}
		return ctrl.Result{}, r.patchStatus(ctx, orig, seq)
	}

	if seq.Status.Phase == "" {
		seq.Status.Phase = schedulingv1alpha1.PodSequencePhasePending
		r.Recorder.Event(seq, corev1.EventTypeNormal, "Initialized", "Pod sequence initialized")
	}

	if scope == schedulingv1alpha1.PodSequenceScopeNode {
		return r.reconcileNodeScoped(ctx, orig, seq)
	}
	return r.reconcileClusterScoped(ctx, orig, seq)
}

// reconcileClusterScoped handles cluster-level pod sequencing
func (r *PodSequenceReconciler) reconcileClusterScoped(ctx context.Context, orig, seq *schedulingv1alpha1.PodSequence) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	targetNS := sequence.TargetNamespace(seq)
	gate := sequence.GateName(seq)
	groups := seq.Spec.PodGroups
	idx := seq.Status.CurrentIndex

	if idx >= len(groups) {
		if seq.Status.Phase != schedulingv1alpha1.PodSequencePhaseCompleted {
			r.Recorder.Event(seq, corev1.EventTypeNormal, "Completed", "All pod groups in sequence are ready")
		}
		seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseCompleted
		seq.Status.Message = "All pod groups in sequence are ready"
		// The sequence is done: never leave member pods created afterwards stuck behind the gate.
		var errs []error
		for _, g := range groups {
			pods, err := r.getPodsInGroup(ctx, g, targetNS)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, p := range pods {
				if sequence.HasGate(p, gate) {
					if err := r.removeGate(ctx, p, gate); err != nil {
						errs = append(errs, err)
					} else {
						metricUngatedPods.WithLabelValues(seq.Namespace, seq.Name, "completed").Inc()
					}
				}
			}
		}
		errs = append(errs, r.patchStatus(ctx, orig, seq))
		return ctrl.Result{}, kerrors.NewAggregate(errs)
	}

	group := groups[idx]
	groupName := sequence.GroupName(group, idx)
	seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress

	pods, err := r.getPodsInGroup(ctx, group, targetNS)
	if err != nil {
		seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseFailed
		seq.Status.Message = fmt.Sprintf("Failed to get pods in %s: %v", groupName, err)
		_ = r.patchStatus(ctx, orig, seq)
		return ctrl.Result{}, err
	}

	if idx > 0 {
		// Groups before idx already passed: release any recreated member pods right away.
		for i := 0; i < idx; i++ {
			passed, err := r.getPodsInGroup(ctx, groups[i], targetNS)
			if err != nil {
				return ctrl.Result{}, err
			}
			for _, p := range passed {
				if sequence.HasGate(p, gate) {
					if err := r.removeGate(ctx, p, gate); err != nil {
						return ctrl.Result{}, err
					}
					metricUngatedPods.WithLabelValues(seq.Namespace, seq.Name, ReasonSatisfied).Inc()
				}
			}
		}

		prev := groups[idx-1]
		prevPods, err := r.getPodsInGroup(ctx, prev, targetNS)
		if err != nil {
			return ctrl.Result{}, err
		}
		for _, p := range prevPods {
			if !sequence.IsPodReady(p) {
				seq.Status.Message = fmt.Sprintf("Waiting for all pods in %s to become ready", sequence.GroupName(prev, idx-1))
				return ctrl.Result{RequeueAfter: RequeueDelay}, r.patchStatus(ctx, orig, seq)
			}
		}
	}

	for _, p := range pods {
		if sequence.HasGate(p, gate) {
			if err := r.removeGate(ctx, p, gate); err != nil {
				return ctrl.Result{}, err
			}
			metricUngatedPods.WithLabelValues(seq.Namespace, seq.Name, ReasonSatisfied).Inc()
		}
	}

	readyCount := 0
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
		if sequence.IsPodReady(p) {
			readyCount++
		}
	}
	seq.Status.CurrentGroupPods = names
	seq.Status.ReadyPodsInCurrentGroup = readyCount

	expected := len(pods)
	if len(group.Pods) > 0 {
		expected = len(group.Pods)
	}
	if len(pods) == 0 || len(pods) < expected {
		seq.Status.Message = fmt.Sprintf("Waiting for pods in %s to be created", groupName)
		return ctrl.Result{RequeueAfter: RequeueDelay}, r.patchStatus(ctx, orig, seq)
	}
	if readyCount < len(pods) {
		seq.Status.Message = fmt.Sprintf("Waiting for all pods in %s to become ready (%d/%d ready)", groupName, readyCount, len(pods))
		return ctrl.Result{RequeueAfter: RequeueDelay}, r.patchStatus(ctx, orig, seq)
	}

	wait, err := sequence.WaitDuration(group)
	if err != nil {
		seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseFailed
		seq.Status.Message = fmt.Sprintf("Invalid wait duration in %s: %v", groupName, err)
		return ctrl.Result{}, r.patchStatus(ctx, orig, seq)
	}
	var remaining time.Duration
	now := r.now()
	for _, p := range pods {
		if rem := sequence.ReadyRemaining(p, wait, now); rem > remaining {
			remaining = rem
		}
	}
	if remaining > 0 {
		seq.Status.Message = fmt.Sprintf("All pods in %s ready, waiting %v before next group", groupName, wait)
		return ctrl.Result{RequeueAfter: remaining}, r.patchStatus(ctx, orig, seq)
	}

	logger.Info("Group ready, advancing", "group", groupName, "pods", len(pods))
	seq.Status.CurrentIndex++
	seq.Status.ProcessedPods = append(seq.Status.ProcessedPods, names...)
	seq.Status.Message = fmt.Sprintf("All pods in %s are ready (%d/%d), moving to next group", groupName, readyCount, len(pods))
	r.Recorder.Event(seq, corev1.EventTypeNormal, "GroupReady", fmt.Sprintf("%s is ready with all %d pods", groupName, len(pods)))
	return ctrl.Result{Requeue: true}, r.patchStatus(ctx, orig, seq)
}

// reconcileNodeScoped evaluates every participating node independently: pods pinned to a node
// (DaemonSet pods) are released when earlier groups are satisfied on that node, and unpinned pods
// are steered with a node label/affinity or taint/tolerations.
func (r *PodSequenceReconciler) reconcileNodeScoped(ctx context.Context, orig, seq *schedulingv1alpha1.PodSequence) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	targetNS := sequence.TargetNamespace(seq)

	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing nodes: %w", err)
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(targetNS)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing pods: %w", err)
	}
	dsList := &appsv1.DaemonSetList{}
	if err := r.List(ctx, dsList, client.InNamespace(targetNS)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing daemonsets: %w", err)
	}

	plan, err := ComputeNodePlan(NodePlanInput{
		Seq: seq, Nodes: nodeList.Items, Pods: podList.Items, DaemonSets: dsList.Items, Now: r.now(),
	})
	if err != nil {
		seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseFailed
		seq.Status.Message = err.Error()
		if orig.Status.Message != seq.Status.Message {
			r.Recorder.Event(seq, corev1.EventTypeWarning, "InvalidSpec", err.Error())
		}
		return ctrl.Result{}, r.patchStatus(ctx, orig, seq)
	}

	var errs []error
	nodesByName := make(map[string]*corev1.Node, len(nodeList.Items))
	for i := range nodeList.Items {
		nodesByName[nodeList.Items[i].Name] = &nodeList.Items[i]
	}
	labelInUse := sequence.PendingRequiredKeys(podList.Items)[plan.Key]
	for _, st := range plan.Nodes {
		if err := r.syncNodeEligibility(ctx, nodesByName[st.Name], plan, st, labelInUse); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", st.Name, err))
		}
	}

	gate := sequence.GateName(seq)
	for _, a := range plan.Ungate {
		if err := r.applyPodAction(ctx, a, gate); err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", a.Pod.Name, err))
			continue
		}
		logger.Info("Removed scheduling gate", "pod", a.Pod.Name, "group", a.Group, "node", sequence.TargetNode(a.Pod), "reason", a.Reason)
		metricUngatedPods.WithLabelValues(seq.Namespace, seq.Name, a.Reason).Inc()
	}

	r.setNodeScopedStatus(orig, seq, plan)
	errs = append(errs, r.patchStatus(ctx, orig, seq))
	if err := kerrors.NewAggregate(errs); err != nil {
		return ctrl.Result{}, err
	}

	requeue := NodeResync
	if plan.RequeueAfter > 0 && plan.RequeueAfter < requeue {
		requeue = plan.RequeueAfter
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *PodSequenceReconciler) setNodeScopedStatus(orig, seq *schedulingv1alpha1.PodSequence, plan *NodePlan) {
	numGroups := plan.NumGroups
	total, satisfied, failed := 0, 0, 0
	minIdx := numGroups
	var failedEntries, pendingEntries []schedulingv1alpha1.NodeSequenceStatus
	for _, st := range plan.Nodes {
		if !st.Participating {
			continue
		}
		total++
		if st.Satisfied < minIdx {
			minIdx = st.Satisfied
		}
		if st.Satisfied >= numGroups {
			satisfied++
			continue
		}
		entry := schedulingv1alpha1.NodeSequenceStatus{
			NodeName: st.Name, CurrentIndex: st.Satisfied, Phase: schedulingv1alpha1.PodSequencePhaseInProgress, Message: st.Message,
		}
		if st.Failed {
			failed++
			entry.Phase = schedulingv1alpha1.PodSequencePhaseFailed
			failedEntries = append(failedEntries, entry)
		} else {
			pendingEntries = append(pendingEntries, entry)
		}
	}
	entries := append(failedEntries, pendingEntries...)
	if len(entries) > sequence.MaxNodeStatusEntries {
		entries = entries[:sequence.MaxNodeStatusEntries]
	}

	s := &seq.Status
	s.NodeEligibilityKey = plan.Key
	s.NodeStatus = entries
	s.Summary = &schedulingv1alpha1.NodeSummary{TotalNodes: total, SatisfiedNodes: satisfied, FailedNodes: failed, GatedPods: plan.GatedPods}
	s.CurrentIndex = minIdx
	s.CurrentGroupPods = nil
	s.ReadyPodsInCurrentGroup = 0
	s.ProcessedPods = nil
	s.Message = fmt.Sprintf("%d/%d nodes satisfied, %d pods gated", satisfied, total, plan.GatedPods)
	if satisfied == total {
		s.Phase = schedulingv1alpha1.PodSequencePhaseActive
	} else {
		s.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
	}

	readyCond := metav1.Condition{Type: schedulingv1alpha1.ConditionReady, ObservedGeneration: seq.Generation}
	if satisfied == total {
		readyCond.Status, readyCond.Reason, readyCond.Message = metav1.ConditionTrue, "AllNodesSatisfied", s.Message
	} else {
		readyCond.Status, readyCond.Reason, readyCond.Message = metav1.ConditionFalse, "NodesPending", s.Message
	}
	meta.SetStatusCondition(&s.Conditions, readyCond)

	degraded := metav1.Condition{Type: schedulingv1alpha1.ConditionDegraded, ObservedGeneration: seq.Generation,
		Status: metav1.ConditionFalse, Reason: "AsExpected", Message: "No problems detected"}
	var problems []string
	if failed > 0 {
		degraded.Reason = "GateTimeout"
		var names []string
		for _, e := range failedEntries {
			names = append(names, e.NodeName)
		}
		problems = append(problems, fmt.Sprintf("%d node(s) exceeded gateTimeout: %s", failed, truncateList(names, 10)))
	}
	if len(plan.MissingToleration) > 0 {
		if degraded.Reason == "AsExpected" {
			degraded.Reason = "MissingToleration"
		}
		problems = append(problems, fmt.Sprintf("DaemonSet(s) %s do not tolerate taint %s; label the DaemonSet with %s so the webhook injects it, or add the toleration",
			strings.Join(plan.MissingToleration, ", "), plan.Key, sequence.SequenceLabel))
	}
	if len(problems) > 0 {
		degraded.Status = metav1.ConditionTrue
		degraded.Message = strings.Join(problems, "; ")
	}
	prev := meta.FindStatusCondition(orig.Status.Conditions, schedulingv1alpha1.ConditionDegraded)
	if degraded.Status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue || prev.Message != degraded.Message) {
		r.Recorder.Event(seq, corev1.EventTypeWarning, degraded.Reason, degraded.Message)
	}
	meta.SetStatusCondition(&s.Conditions, degraded)

	metricNodesTotal.WithLabelValues(seq.Namespace, seq.Name).Set(float64(total))
	metricNodesSatisfied.WithLabelValues(seq.Namespace, seq.Name).Set(float64(satisfied))
	metricNodesFailed.WithLabelValues(seq.Namespace, seq.Name).Set(float64(failed))
	metricGatedPods.WithLabelValues(seq.Namespace, seq.Name).Set(float64(plan.GatedPods))
}

func truncateList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" and %d more", len(items)-max)
}

// syncNodeEligibility makes the node's label/taint for this sequence reflect its satisfied count.
// labelInUse reports whether unscheduled pods still require the label (released in NodeLabel mode).
func (r *PodSequenceReconciler) syncNodeEligibility(ctx context.Context, node *corev1.Node, plan *NodePlan, st NodeState, labelInUse bool) error {
	if node == nil {
		return nil
	}
	wantLabel := ""
	var wantTaint *corev1.Taint
	if st.Participating {
		value := strconv.Itoa(st.Satisfied)
		if plan.Method == schedulingv1alpha1.NodeEligibilityTaint {
			if st.Satisfied < plan.NumGroups {
				wantTaint = &corev1.Taint{Key: plan.Key, Value: value, Effect: corev1.TaintEffectNoSchedule}
			}
		} else {
			wantLabel = value
		}
	}
	if _, has := node.Labels[plan.Key]; has && labelInUse && plan.Method == schedulingv1alpha1.NodeEligibilityTaint {
		// Switched from NodeLabel to Taint: don't strand pods already released with the label affinity.
		wantLabel = sequence.ReleasedLabelValue
	}
	return r.setNodeKey(ctx, node, plan.Key, wantLabel, wantTaint)
}

// setNodeKey ensures the node label (wantLabel "" means absent) and taint (nil means absent) for key.
func (r *PodSequenceReconciler) setNodeKey(ctx context.Context, node *corev1.Node, key, wantLabel string, wantTaint *corev1.Taint) error {
	cur, has := node.Labels[key]
	if (wantLabel == "" && has) || (wantLabel != "" && cur != wantLabel) {
		var v interface{}
		if wantLabel != "" {
			v = wantLabel
		}
		if err := r.patchNodeLabels(ctx, node, map[string]interface{}{key: v}); err != nil {
			return err
		}
	}

	var newTaints []corev1.Taint
	changed, found := false, false
	for _, t := range node.Spec.Taints {
		if t.Key != key {
			newTaints = append(newTaints, t)
			continue
		}
		if wantTaint != nil && !found && t.Value == wantTaint.Value && t.Effect == wantTaint.Effect {
			newTaints = append(newTaints, t)
			found = true
			continue
		}
		changed = true
	}
	if wantTaint != nil && !found {
		newTaints = append(newTaints, *wantTaint)
		changed = true
	}
	if !changed {
		return nil
	}
	return r.patchNodeTaints(ctx, node, newTaints)
}

// releaseNodeKeys removes every taint whose key is stale. Stale labels are removed too, unless an
// unscheduled pod still requires them (inUse), in which case they are set to ReleasedLabelValue so the
// pod can schedule; a later cleanup removes them once nothing references them.
func (r *PodSequenceReconciler) releaseNodeKeys(ctx context.Context, node *corev1.Node, stale func(string) bool, inUse map[string]bool) error {
	labelPatch := map[string]interface{}{}
	for k, v := range node.Labels {
		if !stale(k) {
			continue
		}
		if inUse[k] {
			if v != sequence.ReleasedLabelValue {
				labelPatch[k] = sequence.ReleasedLabelValue
			}
		} else {
			labelPatch[k] = nil
		}
	}
	if len(labelPatch) > 0 {
		if err := r.patchNodeLabels(ctx, node, labelPatch); err != nil {
			return err
		}
	}
	var newTaints []corev1.Taint
	for _, t := range node.Spec.Taints {
		if !stale(t.Key) {
			newTaints = append(newTaints, t)
		}
	}
	if len(newTaints) == len(node.Spec.Taints) {
		return nil
	}
	return r.patchNodeTaints(ctx, node, newTaints)
}

func (r *PodSequenceReconciler) patchNodeLabels(ctx context.Context, node *corev1.Node, labelPatch map[string]interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": labelPatch}})
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Patch(ctx, node, client.RawPatch(types.MergePatchType, body)))
}

// patchNodeTaints replaces the taint list using an optimistic lock so concurrent writers are not lost.
func (r *PodSequenceReconciler) patchNodeTaints(ctx context.Context, node *corev1.Node, taints []corev1.Taint) error {
	orig := node.DeepCopy()
	node.Spec.Taints = taints
	return client.IgnoreNotFound(r.Patch(ctx, node, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})))
}

// applyPodAction adds the eligibility constraints (while the pod is still gated) and removes the gate.
func (r *PodSequenceReconciler) applyPodAction(ctx context.Context, a PodAction, gate string) error {
	pod := a.Pod
	if len(a.AddTolerations) > 0 || a.AddAffinity != nil {
		orig := pod.DeepCopy()
		changed := sequence.AddTolerations(&pod.Spec, a.AddTolerations)
		if a.AddAffinity != nil && sequence.AddRequiredNodeAffinity(&pod.Spec, *a.AddAffinity) {
			changed = true
		}
		if changed {
			if err := r.Patch(ctx, pod, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
				return client.IgnoreNotFound(err)
			}
		}
	}
	return r.removeGate(ctx, pod, gate)
}

// removeGate removes the scheduling gate from a pod with an optimistic-lock patch.
func (r *PodSequenceReconciler) removeGate(ctx context.Context, pod *corev1.Pod, gate string) error {
	if !sequence.HasGate(pod, gate) {
		return nil
	}
	orig := pod.DeepCopy()
	var gates []corev1.PodSchedulingGate
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name != gate {
			gates = append(gates, g)
		}
	}
	pod.Spec.SchedulingGates = gates
	if err := r.Patch(ctx, pod, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

func (r *PodSequenceReconciler) patchStatus(ctx context.Context, orig, seq *schedulingv1alpha1.PodSequence) error {
	if equality.Semantic.DeepEqual(orig.Status, seq.Status) {
		return nil
	}
	// A full update (not a merge patch) so required status fields are always sent. On conflict the
	// newer object triggers another reconcile through the watch.
	err := r.Status().Update(ctx, seq)
	if apierrors.IsConflict(err) {
		return nil
	}
	return client.IgnoreNotFound(err)
}

// getPodsInGroup returns all pods in a group, either by explicit names or label selector
func (r *PodSequenceReconciler) getPodsInGroup(ctx context.Context, group schedulingv1alpha1.PodGroup, targetNamespace string) ([]*corev1.Pod, error) {
	var pods []*corev1.Pod
	if len(group.Pods) > 0 {
		for _, podName := range group.Pods {
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: targetNamespace, Name: podName}, pod); err != nil {
				if client.IgnoreNotFound(err) == nil {
					continue
				}
				return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
			}
			pods = append(pods, pod)
		}
		return pods, nil
	}
	if group.PodSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(group.PodSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList, client.InNamespace(targetNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, fmt.Errorf("failed to list pods: %w", err)
		}
		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
		return pods, nil
	}
	return nil, fmt.Errorf("group must specify either pods or podSelector")
}

// handleDeletion releases every pod still gated by the sequence (fail open), removes node
// labels/taints and finally the finalizer.
func (r *PodSequenceReconciler) handleDeletion(ctx context.Context, seq *schedulingv1alpha1.PodSequence) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(seq, FinalizerKey) {
		return ctrl.Result{}, nil
	}
	var errs []error

	gate := sequence.GateName(seq)
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(sequence.TargetNamespace(seq))); err != nil {
		errs = append(errs, err)
	}
	for i := range podList.Items {
		p := &podList.Items[i]
		if !sequence.HasGate(p, gate) {
			continue
		}
		if sequence.GroupIndexForPod(seq, p) >= 0 || p.Labels[sequence.SequenceLabel] == seq.Name {
			if err := r.removeGate(ctx, p, gate); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if sequence.Scope(seq) == schedulingv1alpha1.PodSequenceScopeNode && r.NodeScopeEnabled {
		key := sequence.EligibilityKey(seq.Namespace, seq.Name)
		inUse := sequence.PendingRequiredKeys(podList.Items)
		nodeList := &corev1.NodeList{}
		if err := r.List(ctx, nodeList); err != nil {
			errs = append(errs, err)
		}
		for i := range nodeList.Items {
			if err := r.releaseNodeKeys(ctx, &nodeList.Items[i], func(k string) bool { return k == key }, inUse); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := kerrors.NewAggregate(errs); err != nil {
		return ctrl.Result{}, err
	}

	deleteMetrics(seq.Namespace, seq.Name)
	controllerutil.RemoveFinalizer(seq, FinalizerKey)
	if err := r.Update(ctx, seq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	r.Recorder.Event(seq, corev1.EventTypeNormal, "Deleted", "Pod sequence cleaned up successfully")
	return ctrl.Result{}, nil
}

// cleanupOrphanedNodeKeys removes eligibility labels/taints that no node-scoped PodSequence manages
// (deleted sequences), plus per-group taints created by earlier versions of this controller. Labels
// still required by unscheduled pods are kept at ReleasedLabelValue until those pods schedule.
func (r *PodSequenceReconciler) cleanupOrphanedNodeKeys(ctx context.Context) error {
	seqList := &schedulingv1alpha1.PodSequenceList{}
	if err := r.List(ctx, seqList); err != nil {
		return err
	}
	valid := map[string]bool{}
	for i := range seqList.Items {
		if sequence.Scope(&seqList.Items[i]) == schedulingv1alpha1.PodSequenceScopeNode {
			valid[sequence.EligibilityKey(seqList.Items[i].Namespace, seqList.Items[i].Name)] = true
		}
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return err
	}
	inUse := sequence.PendingRequiredKeys(podList.Items)
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return err
	}
	var errs []error
	for i := range nodeList.Items {
		err := r.releaseNodeKeys(ctx, &nodeList.Items[i], func(k string) bool {
			return (sequence.IsEligibilityKey(k) && !valid[k]) || sequence.IsLegacyGroupTaintKey(k)
		}, inUse)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return kerrors.NewAggregate(errs)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodSequenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&schedulingv1alpha1.PodSequence{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPod))

	if r.NodeScopeEnabled {
		if err := mgr.Add(&orphanCleaner{reconciler: r, mgr: mgr}); err != nil {
			return err
		}
		b = b.
			Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNode), builder.WithPredicates(nodeChangePredicate())).
			Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(r.mapDaemonSet))
	}
	return b.Complete(r)
}

func (r *PodSequenceReconciler) listSequences(ctx context.Context) []schedulingv1alpha1.PodSequence {
	list := &schedulingv1alpha1.PodSequenceList{}
	if err := r.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "listing PodSequences for watch mapping")
		return nil
	}
	return list.Items
}

func requestFor(seq *schedulingv1alpha1.PodSequence) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: seq.Namespace, Name: seq.Name}}
}

func (r *PodSequenceReconciler) mapPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	var reqs []reconcile.Request
	seqs := r.listSequences(ctx)
	for i := range seqs {
		seq := &seqs[i]
		if sequence.TargetNamespace(seq) != pod.Namespace {
			continue
		}
		if pod.Labels[sequence.SequenceLabel] == seq.Name || sequence.GroupIndexForPod(seq, pod) >= 0 {
			reqs = append(reqs, requestFor(seq))
		}
	}
	return reqs
}

func (r *PodSequenceReconciler) mapNode(ctx context.Context, _ client.Object) []reconcile.Request {
	var reqs []reconcile.Request
	seqs := r.listSequences(ctx)
	for i := range seqs {
		if sequence.Scope(&seqs[i]) == schedulingv1alpha1.PodSequenceScopeNode {
			reqs = append(reqs, requestFor(&seqs[i]))
		}
	}
	return reqs
}

func (r *PodSequenceReconciler) mapDaemonSet(ctx context.Context, obj client.Object) []reconcile.Request {
	ds, ok := obj.(*appsv1.DaemonSet)
	if !ok {
		return nil
	}
	var reqs []reconcile.Request
	seqs := r.listSequences(ctx)
	for i := range seqs {
		seq := &seqs[i]
		if sequence.Scope(seq) != schedulingv1alpha1.PodSequenceScopeNode || sequence.TargetNamespace(seq) != ds.Namespace {
			continue
		}
		match := ds.Labels[sequence.SequenceLabel] == seq.Name
		for _, g := range seq.Spec.PodGroups {
			if sel, err := sequence.GroupSelector(g); err == nil && sel != nil && sel.Matches(labels.Set(ds.Spec.Template.Labels)) {
				match = true
			}
		}
		if match {
			reqs = append(reqs, requestFor(seq))
		}
	}
	return reqs
}

// nodeChangePredicate ignores node status heartbeats; only label/taint changes, creates and deletes matter.
func nodeChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*corev1.Node)
			n, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			return !equality.Semantic.DeepEqual(o.Labels, n.Labels) || !equality.Semantic.DeepEqual(o.Spec.Taints, n.Spec.Taints)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// orphanCleaner periodically removes stale node labels/taints. Runs only on the leader.
type orphanCleaner struct {
	reconciler *PodSequenceReconciler
	mgr        ctrl.Manager
}

func (o *orphanCleaner) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("orphan-cleaner")
	if !o.mgr.GetCache().WaitForCacheSync(ctx) {
		return fmt.Errorf("failed to wait for cache sync")
	}
	run := func() {
		if err := o.reconciler.cleanupOrphanedNodeKeys(ctx); err != nil {
			logger.Error(err, "orphaned node label/taint cleanup failed")
		}
	}
	run()
	ticker := time.NewTicker(OrphanCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}
