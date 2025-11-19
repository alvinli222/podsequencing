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

	schedulingv1alpha1 "github.com/example/pod-sequence-controller/api/v1alpha1"
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

	// Determine if we're using PodGroups or legacy Sequence
	usePodGroups := len(podSeq.Spec.PodGroups) > 0

	// Initialize status if needed
	if podSeq.Status.Phase == "" {
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhasePending
		podSeq.Status.CurrentIndex = 0
		podSeq.Status.ProcessedPods = []string{}
		podSeq.Status.CurrentGroupPods = []string{}
		podSeq.Status.ReadyPodsInCurrentGroup = 0
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
	log.Info("Processing pod group", "index", currentIndex, "groupName", groupName, "podCount", len(currentGroup.Pods))

	// Check if all pods in previous group are ready (if not first group)
	if currentIndex > 0 {
		prevGroup := podSeq.Spec.PodGroups[currentIndex-1]
		allPreviousReady := true
		for _, podName := range prevGroup.Pods {
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: targetNamespace, Name: podName}
			if err := r.Get(ctx, podKey, pod); err != nil {
				log.Error(err, "Failed to get pod from previous group", "podName", podName)
				return ctrl.Result{}, err
			}
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
	for _, podName := range currentGroup.Pods {
		pod := &corev1.Pod{}
		podKey := types.NamespacedName{Namespace: targetNamespace, Name: podName}
		if err := r.Get(ctx, podKey, pod); err != nil {
			if errors.IsNotFound(err) {
				podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseFailed
				podSeq.Status.Message = fmt.Sprintf("Pod %s not found in namespace %s", podName, targetNamespace)
				r.Recorder.Event(podSeq, corev1.EventTypeWarning, "PodNotFound", podSeq.Status.Message)
				if err := r.Status().Update(ctx, podSeq); err != nil {
					log.Error(err, "Failed to update status")
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
			log.Error(err, "Failed to get pod", "podName", podName)
			return ctrl.Result{}, err
		}

		if err := r.removeSchedulingGate(ctx, pod, schedulingGateName); err != nil {
			log.Error(err, "Failed to remove scheduling gate", "podName", podName)
			return ctrl.Result{}, err
		}
	}

	// Check how many pods in the current group are ready
	readyCount := 0
	for _, podName := range currentGroup.Pods {
		pod := &corev1.Pod{}
		podKey := types.NamespacedName{Namespace: targetNamespace, Name: podName}
		if err := r.Get(ctx, podKey, pod); err != nil {
			log.Error(err, "Failed to get pod", "podName", podName)
			return ctrl.Result{}, err
		}
		if isPodReady(pod) {
			readyCount++
		}
	}

	// Update status with current group info
	podSeq.Status.CurrentGroupPods = currentGroup.Pods
	podSeq.Status.ReadyPodsInCurrentGroup = readyCount

	// Check if all pods in current group are ready
	if readyCount == len(currentGroup.Pods) {
		// All pods in group are ready, move to next group
		podSeq.Status.CurrentIndex++
		podSeq.Status.ProcessedPods = append(podSeq.Status.ProcessedPods, currentGroup.Pods...)
		podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
		podSeq.Status.Message = fmt.Sprintf("All pods in %s are ready (%d/%d), moving to next group", groupName, readyCount, len(currentGroup.Pods))
		if err := r.Status().Update(ctx, podSeq); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		r.Recorder.Event(podSeq, corev1.EventTypeNormal, "GroupReady", fmt.Sprintf("%s is ready with all %d pods", groupName, len(currentGroup.Pods)))
		return ctrl.Result{Requeue: true}, nil
	}

	// Not all pods ready yet, wait
	log.Info("Not all pods in group ready yet, waiting", "groupName", groupName, "ready", readyCount, "total", len(currentGroup.Pods))
	podSeq.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
	podSeq.Status.Message = fmt.Sprintf("Waiting for all pods in %s to become ready (%d/%d ready)", groupName, readyCount, len(currentGroup.Pods))
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

// SetupWithManager sets up the controller with the Manager.
func (r *PodSequenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&schedulingv1alpha1.PodSequence{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
