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

// reconcileNodeScoped handles node-level pod sequencing
func (r *PodSequenceReconciler) reconcileNodeScoped(ctx context.Context, podSeq *schedulingv1alpha1.PodSequence, targetNamespace, schedulingGateName string) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get all pods across all groups to determine which nodes are involved
	allNodeNames := make(map[string]bool)
	for _, group := range podSeq.Spec.PodGroups {
		pods, err := r.getPodsInGroup(ctx, group, targetNamespace)
		if err != nil {
			log.Error(err, "Failed to get pods in group")
			continue
		}
		for _, pod := range pods {
			// Track node if pod is scheduled
			if pod.Spec.NodeName != "" {
				allNodeNames[pod.Spec.NodeName] = true
			}
		}
	}

	// Initialize node status if needed
	nodeStatusMap := make(map[string]*schedulingv1alpha1.NodeSequenceStatus)
	for _, ns := range podSeq.Status.NodeStatus {
		nodeStatusMap[ns.NodeName] = &schedulingv1alpha1.NodeSequenceStatus{
			NodeName:                ns.NodeName,
			CurrentIndex:            ns.CurrentIndex,
			ReadyPodsInCurrentGroup: ns.ReadyPodsInCurrentGroup,
			Phase:                   ns.Phase,
		}
	}

	// Process each node independently
	needsRequeue := false
	allNodesCompleted := true
	minIndex := len(podSeq.Spec.PodGroups)

	for nodeName := range allNodeNames {
		// Get or create node status
		nodeStatus, exists := nodeStatusMap[nodeName]
		if !exists {
			nodeStatus = &schedulingv1alpha1.NodeSequenceStatus{
				NodeName:                nodeName,
				CurrentIndex:            0,
				ReadyPodsInCurrentGroup: 0,
				Phase:                   schedulingv1alpha1.PodSequencePhasePending,
			}
			nodeStatusMap[nodeName] = nodeStatus
		}

		// Check if this node is completed
		if nodeStatus.CurrentIndex >= len(podSeq.Spec.PodGroups) {
			nodeStatus.Phase = schedulingv1alpha1.PodSequencePhaseCompleted
			continue
		}

		allNodesCompleted = false
		if nodeStatus.CurrentIndex < minIndex {
			minIndex = nodeStatus.CurrentIndex
		}

		currentGroup := podSeq.Spec.PodGroups[nodeStatus.CurrentIndex]
		groupName := currentGroup.Name
		if groupName == "" {
			groupName = fmt.Sprintf("Group %d", nodeStatus.CurrentIndex+1)
		}

		// Check if previous group is ready on this node
		if nodeStatus.CurrentIndex > 0 {
			prevGroup := podSeq.Spec.PodGroups[nodeStatus.CurrentIndex-1]
			prevPods, err := r.getPodsInGroup(ctx, prevGroup, targetNamespace)
			if err != nil {
				log.Error(err, "Failed to get pods in previous group", "node", nodeName)
				return ctrl.Result{}, err
			}

			allPreviousReady := true
			for _, pod := range prevPods {
				// Only check pods on this specific node
				if pod.Spec.NodeName != nodeName {
					continue
				}
				if !isPodReady(pod) {
					allPreviousReady = false
					break
				}
			}

			if !allPreviousReady {
				log.Info("Previous group not ready on node, waiting", "node", nodeName, "group", groupName)
				nodeStatus.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
				needsRequeue = true
				continue
			}
		}

		// Get pods in current group
		currentPods, err := r.getPodsInGroup(ctx, currentGroup, targetNamespace)
		if err != nil {
			log.Error(err, "Failed to get pods in current group", "node", nodeName)
			return ctrl.Result{}, err
		}

		// Remove scheduling gates from pods in current group on this node
		for _, pod := range currentPods {
			// Only process pods on this specific node
			if pod.Spec.NodeName != nodeName {
				continue
			}

			if err := r.removeSchedulingGate(ctx, pod, schedulingGateName); err != nil {
				log.Error(err, "Failed to remove scheduling gate", "podName", pod.Name, "node", nodeName)
				return ctrl.Result{}, err
			}
		}

		// Check how many pods in current group are ready on this node
		readyCount := 0
		totalPodsOnNode := 0
		for _, pod := range currentPods {
			if pod.Spec.NodeName != nodeName {
				continue
			}
			totalPodsOnNode++
			if isPodReady(pod) {
				readyCount++
			}
		}

		nodeStatus.ReadyPodsInCurrentGroup = readyCount

		// Check if all pods in current group on this node are ready
		if totalPodsOnNode > 0 && readyCount == totalPodsOnNode {
			// All pods on this node are ready, move to next group
			nodeStatus.CurrentIndex++
			nodeStatus.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
			log.Info("Node completed group", "node", nodeName, "group", groupName, "ready", readyCount)
			needsRequeue = true
		} else {
			// Not all pods ready yet
			nodeStatus.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
			log.Info("Waiting for pods on node", "node", nodeName, "group", groupName, "ready", readyCount, "total", totalPodsOnNode)
			needsRequeue = true
		}
	}

	// Update status with all node statuses
	podSeq.Status.NodeStatus = []schedulingv1alpha1.NodeSequenceStatus{}
	for _, ns := range nodeStatusMap {
		podSeq.Status.NodeStatus = append(podSeq.Status.NodeStatus, *ns)
	}
	podSeq.Status.CurrentIndex = minIndex

	if allNodesCompleted {
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseCompleted
		podSeq.Status.Message = "All nodes completed their sequences"
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "Completed", "All nodes completed their sequences")
	} else {
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = fmt.Sprintf("Processing node-scoped sequences (min group index: %d)", minIndex)
	}

	if err := r.Status().Update(ctx, podSeq); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	if needsRequeue {
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}
	return ctrl.Result{}, nil
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
