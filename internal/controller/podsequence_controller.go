package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
)

const (
	DefaultSchedulingGate = "podsequence.example.com/sequence-gate"
	RequeueDelay          = 5 * time.Second
)

// PodSequenceReconciler reconciles a PodSequence object
type PodSequenceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=scheduling.example.com,resources=podsequences,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scheduling.example.com,resources=podsequences/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scheduling.example.com,resources=podsequences/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *PodSequenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the PodSequence instance
	podSeq := &schedulingv1alpha1.PodSequence{}
	if err := r.Get(ctx, req.NamespacedName, podSeq); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get PodSequence")
		return ctrl.Result{}, err
	}

	// Determine the target namespace
	targetNamespace := podSeq.Spec.Namespace
	if targetNamespace == "" {
		targetNamespace = podSeq.Namespace
	}

	// Determine the scheduling gate name
	schedulingGateName := podSeq.Spec.SchedulingGateName
	if schedulingGateName == "" {
		schedulingGateName = DefaultSchedulingGate
	}

	// Determine scope (default to Cluster if not specified)
	scope := podSeq.Spec.Scope
	if scope == "" {
		scope = schedulingv1alpha1.PodSequenceScopeCluster
	}

	// Initialize status if needed
	if podSeq.Status.Phase == "" {
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhasePending
		podSeq.Status.CurrentIndex = 0
		podSeq.Status.ProcessedPods = []string{}
		podSeq.Status.CurrentGroupPods = []string{}
		podSeq.Status.ReadyPodsInCurrentGroup = 0
		if scope == schedulingv1alpha1.PodSequenceScopeNode {
			podSeq.Status.NodeStatus = []schedulingv1alpha1.NodeSequenceStatus{}
		}
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to initialize status")
			return ctrl.Result{}, err
		}
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "Initialized", "Pod sequence initialized")
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if sequence is already completed
	if podSeq.Status.Phase == schedulingv1alpha1.PodSequencePhaseCompleted {
		return ctrl.Result{}, nil
	}

	// Route to appropriate reconciliation based on scope
	if scope == schedulingv1alpha1.PodSequenceScopeNode {
		return r.reconcileNodeScoped(ctx, podSeq, targetNamespace, schedulingGateName)
	}
	return r.reconcileClusterScoped(ctx, podSeq, targetNamespace, schedulingGateName)
}

// reconcileClusterScoped handles cluster-level pod sequencing
func (r *PodSequenceReconciler) reconcileClusterScoped(ctx context.Context, podSeq *schedulingv1alpha1.PodSequence, targetNamespace, schedulingGateName string) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Process pod groups

	currentIndex := podSeq.Status.CurrentIndex
	if currentIndex >= len(podSeq.Spec.PodGroups) {
		// All groups processed
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseCompleted
		podSeq.Status.Message = "All pod groups in sequence are ready"
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "Completed", "All pod groups in sequence are ready")
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status to completed")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	currentGroup := podSeq.Spec.PodGroups[currentIndex]
	groupName := currentGroup.Name
	if groupName == "" {
		groupName = fmt.Sprintf("Group %d", currentIndex+1)
	}

	// Get pods in current group (by name or selector)
	currentPods, err := r.getPodsInGroup(ctx, currentGroup, targetNamespace)
	if err != nil {
		log.Error(err, "Failed to get pods in current group")
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseFailed
		podSeq.Status.Message = fmt.Sprintf("Failed to get pods in %s: %v", groupName, err)
		r.Status().Update(ctx, podSeq)
		return ctrl.Result{}, err
	}

	log.Info("Processing pod group", "index", currentIndex, "groupName", groupName, "podCount", len(currentPods))

	// Check if all pods in previous group are ready (if not first group)
	if currentIndex > 0 {
		prevGroup := podSeq.Spec.PodGroups[currentIndex-1]
		prevPods, err := r.getPodsInGroup(ctx, prevGroup, targetNamespace)
		if err != nil {
			log.Error(err, "Failed to get pods in previous group")
			return ctrl.Result{}, err
		}

		allPreviousReady := true
		for _, pod := range prevPods {
			if !isPodReady(pod) {
				allPreviousReady = false
				break
			}
		}

		if !allPreviousReady {
			prevGroupName := prevGroup.Name
			if prevGroupName == "" {
				prevGroupName = fmt.Sprintf("Group %d", currentIndex)
			}
			log.Info("Previous group not fully ready yet, waiting", "prevGroup", prevGroupName)
			podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
			podSeq.Status.Message = fmt.Sprintf("Waiting for all pods in %s to become ready", prevGroupName)
			if err := r.Status().Update(ctx, podSeq); err != nil {
				log.Error(err, "Failed to update status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: RequeueDelay}, nil
		}
	}

	// Remove scheduling gates from all pods in current group
	for _, pod := range currentPods {
		if err := r.removeSchedulingGate(ctx, pod, schedulingGateName); err != nil {
			log.Error(err, "Failed to remove scheduling gate", "podName", pod.Name)
			return ctrl.Result{}, err
		}
	}

	// Check how many pods in the current group are ready
	readyCount := 0
	currentPodNames := []string{}
	for _, pod := range currentPods {
		currentPodNames = append(currentPodNames, pod.Name)
		if isPodReady(pod) {
			readyCount++
		}
	}

	// Update status with current group info
	podSeq.Status.CurrentGroupPods = currentPodNames
	podSeq.Status.ReadyPodsInCurrentGroup = readyCount

	// If no pods were found, wait for them to be created
	if len(currentPods) == 0 {
		log.Info("No pods found in current group, waiting for pods to be created", "groupName", groupName)
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = fmt.Sprintf("Waiting for pods in %s to be created", groupName)
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}

	// Check if all pods in current group are ready
	if readyCount == len(currentPods) {
		// All pods in group are ready, move to next group
		podSeq.Status.CurrentIndex++
		podSeq.Status.ProcessedPods = append(podSeq.Status.ProcessedPods, currentPodNames...)
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = fmt.Sprintf("All pods in %s are ready (%d/%d), moving to next group", groupName, readyCount, len(currentPods))
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "GroupReady", fmt.Sprintf("%s is ready with all %d pods", groupName, len(currentPods)))
		return ctrl.Result{Requeue: true}, nil
	}

	// Not all pods ready yet, wait
	log.Info("Not all pods in group ready yet, waiting", "groupName", groupName, "ready", readyCount, "total", len(currentPods))
	podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
	podSeq.Status.Message = fmt.Sprintf("Waiting for all pods in %s to become ready (%d/%d ready)", groupName, readyCount, len(currentPods))
	if err := r.Status().Update(ctx, podSeq); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueDelay}, nil
}

// reconcileNodeScoped handles node-level pod sequencing with taints
func (r *PodSequenceReconciler) reconcileNodeScoped(ctx context.Context, podSeq *schedulingv1alpha1.PodSequence, targetNamespace, schedulingGateName string) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Step 1: Initialize - taint all nodes for all groups except the first
	if len(podSeq.Status.NodeStatus) == 0 && len(podSeq.Spec.PodGroups) > 1 {
		// Get all nodes
		nodeList := &corev1.NodeList{}
		if err := r.List(ctx, nodeList); err != nil {
			log.Error(err, "Failed to list nodes")
			return ctrl.Result{}, err
		}

		// Taint all nodes with group-level taints (skip group 0)
		// Use PodSequence name in taint key to avoid conflicts with other sequences
		for i := 1; i < len(podSeq.Spec.PodGroups); i++ {
			taintKey := fmt.Sprintf("podsequence.example.com/%s-group-%d-blocked", podSeq.Name, i)
			for _, node := range nodeList.Items {
				nodeCopy := node.DeepCopy()
				// Check if taint already exists
				taintExists := false
				for _, taint := range nodeCopy.Spec.Taints {
					if taint.Key == taintKey {
						taintExists = true
						break
					}
				}
				
				if !taintExists {
					nodeCopy.Spec.Taints = append(nodeCopy.Spec.Taints, corev1.Taint{
						Key:    taintKey,
						Effect: corev1.TaintEffectNoSchedule,
					})
					if err := r.Update(ctx, nodeCopy); err != nil {
						log.Error(err, "Failed to taint node", "node", node.Name, "taint", taintKey)
						return ctrl.Result{}, err
					}
					log.Info("Tainted node for group sequencing", "node", node.Name, "taint", taintKey)
				}
			}
		}

		// Initialize node status to mark tainting is complete
		podSeq.Status.NodeStatus = []schedulingv1alpha1.NodeSequenceStatus{}
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = "Initialized node taints for sequencing"
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to initialize status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}

	// Step 2: Process groups sequentially
	currentIndex := 0
	if len(podSeq.Status.NodeStatus) > 0 {
		// Find minimum index across all nodes
		minIndex := len(podSeq.Spec.PodGroups)
		for _, ns := range podSeq.Status.NodeStatus {
			if ns.CurrentIndex < minIndex {
				minIndex = ns.CurrentIndex
			}
		}
		currentIndex = minIndex
	}

	// Check if completed
	if currentIndex >= len(podSeq.Spec.PodGroups) {
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseCompleted
		podSeq.Status.Message = "All pod groups completed"
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "Completed", "All nodes completed their sequences")
		return ctrl.Result{}, nil
	}

	currentGroup := podSeq.Spec.PodGroups[currentIndex]
	groupName := currentGroup.Name
	if groupName == "" {
		groupName = fmt.Sprintf("Group %d", currentIndex+1)
	}

	// Get pods in current group
	currentPods, err := r.getPodsInGroup(ctx, currentGroup, targetNamespace)
	if err != nil {
		log.Error(err, "Failed to get pods in current group")
		return ctrl.Result{}, err
	}

	if len(currentPods) == 0 {
		log.Info("No pods found in current group, waiting", "groupName", groupName)
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = fmt.Sprintf("Waiting for pods in %s to be created", groupName)
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}

	// Remove scheduling gates from ALL pods in current group
	for _, pod := range currentPods {
		if err := r.removeSchedulingGate(ctx, pod, schedulingGateName); err != nil {
			log.Error(err, "Failed to remove scheduling gate", "podName", pod.Name)
			return ctrl.Result{}, err
		}
	}

	// Build node status map - track which nodes have ready pods
	nodeStatusMap := make(map[string]*schedulingv1alpha1.NodeSequenceStatus)
	for _, pod := range currentPods {
		if pod.Spec.NodeName == "" {
			continue // Not scheduled yet
		}
		
		nodeName := pod.Spec.NodeName
		if _, exists := nodeStatusMap[nodeName]; !exists {
			nodeStatusMap[nodeName] = &schedulingv1alpha1.NodeSequenceStatus{
				NodeName:                nodeName,
				CurrentIndex:            currentIndex,
				ReadyPodsInCurrentGroup: 0,
				Phase:                   schedulingv1alpha1.PodSequencePhaseInProgress,
			}
		}
		
		// Count ready pods on this node
		if isPodReady(pod) {
			nodeStatusMap[nodeName].ReadyPodsInCurrentGroup++
		}
	}

	// For each node with ready pods in current group, remove the NEXT group's taint
	if currentIndex+1 < len(podSeq.Spec.PodGroups) {
		nextGroupTaint := fmt.Sprintf("podsequence.example.com/%s-group-%d-blocked", podSeq.Name, currentIndex+1)
		
		for nodeName, nodeStatus := range nodeStatusMap {
			if nodeStatus.ReadyPodsInCurrentGroup > 0 {
				// This node has at least one ready pod in current group
				// Remove the next group's taint from this node
				node := &corev1.Node{}
				if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
					log.Error(err, "Failed to get node", "node", nodeName)
					continue
				}

				// Check if taint exists and remove it
				taintRemoved := false
				newTaints := []corev1.Taint{}
				for _, taint := range node.Spec.Taints {
					if taint.Key == nextGroupTaint {
						taintRemoved = true
						log.Info("Removing taint from node", "node", nodeName, "taint", nextGroupTaint)
					} else {
						newTaints = append(newTaints, taint)
					}
				}

				if taintRemoved {
					node.Spec.Taints = newTaints
					if err := r.Update(ctx, node); err != nil {
						log.Error(err, "Failed to update node taints", "node", nodeName)
						return ctrl.Result{}, err
					}
				}
			}
		}
	}

	// Check if ALL pods in current group are ready (on any node)
	allReady := true
	readyCount := 0
	for _, pod := range currentPods {
		if isPodReady(pod) {
			readyCount++
		} else {
			allReady = false
		}
	}

	// Update status
	podSeq.Status.NodeStatus = []schedulingv1alpha1.NodeSequenceStatus{}
	for _, ns := range nodeStatusMap {
		if allReady {
			ns.CurrentIndex++ // Move to next group
		}
		podSeq.Status.NodeStatus = append(podSeq.Status.NodeStatus, *ns)
	}
	podSeq.Status.CurrentIndex = currentIndex

	if allReady {
		log.Info("All pods in group ready, advancing", "groupName", groupName, "ready", readyCount, "total", len(currentPods))
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = fmt.Sprintf("All pods in %s are ready, moving to next group", groupName)
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "GroupReady", fmt.Sprintf("%s is ready with all %d pods", groupName, len(currentPods)))
		return ctrl.Result{Requeue: true}, nil
	}

	// Not all ready yet
	log.Info("Waiting for all pods in group to become ready", "groupName", groupName, "ready", readyCount, "total", len(currentPods))
	podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
	podSeq.Status.Message = fmt.Sprintf("Waiting for all pods in %s to become ready (%d/%d ready)", groupName, readyCount, len(currentPods))
	if err := r.Status().Update(ctx, podSeq); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueDelay}, nil
}

// removeSchedulingGate removes the specified scheduling gate from a pod
func (r *PodSequenceReconciler) removeSchedulingGate(ctx context.Context, pod *corev1.Pod, gateName string) error {
	log := log.FromContext(ctx)

	// Check if the pod has the scheduling gate
	hasGate := false
	newGates := []corev1.PodSchedulingGate{}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == gateName {
			hasGate = true
		} else {
			newGates = append(newGates, gate)
		}
	}

	if !hasGate {
		// Gate already removed
		log.Info("Scheduling gate already removed", "podName", pod.Name, "gateName", gateName)
		return nil
	}

	// Update pod to remove the gate
	pod.Spec.SchedulingGates = newGates
	if err := r.Update(ctx, pod); err != nil {
		return fmt.Errorf("failed to update pod %s: %w", pod.Name, err)
	}

	log.Info("Removed scheduling gate", "podName", pod.Name, "gateName", gateName)
	return nil
}

// isPodReady checks if a pod is in the Ready condition
func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// getPodsInGroup returns all pods in a group, either by explicit names or label selector
func (r *PodSequenceReconciler) getPodsInGroup(ctx context.Context, group schedulingv1alpha1.PodGroup, targetNamespace string) ([]*corev1.Pod, error) {
	log := log.FromContext(ctx)
	var pods []*corev1.Pod

	// If explicit pod names are provided, get them by name
	if len(group.Pods) > 0 {
		for _, podName := range group.Pods {
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: targetNamespace, Name: podName}
			if err := r.Get(ctx, podKey, pod); err != nil {
				if errors.IsNotFound(err) {
					continue // Pod might not exist yet
				}
				return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
			}
			pods = append(pods, pod)
		}
		return pods, nil
	}

	// If podSelector is provided, list pods by label selector
	if group.PodSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(group.PodSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}

		podList := &corev1.PodList{}
		listOpts := &client.ListOptions{
			Namespace:     targetNamespace,
			LabelSelector: selector,
		}
		if err := r.List(ctx, podList, listOpts); err != nil {
			return nil, fmt.Errorf("failed to list pods: %w", err)
		}

		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
		log.Info("Found pods using selector", "count", len(pods), "selector", selector.String())
		return pods, nil
	}

	return pods, fmt.Errorf("group must specify either pods or podSelector")
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodSequenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&schedulingv1alpha1.PodSequence{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
