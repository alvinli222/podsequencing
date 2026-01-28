package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod.podsequence.example.com,admissionReviewVersions=v1

// PodSequenceMutator handles pod mutation for PodSequence tolerations
type PodSequenceMutator struct {
	Client  client.Client
	decoder *admission.Decoder
}

// Handle processes admission requests
func (m *PodSequenceMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := log.FromContext(ctx)
	pod := &corev1.Pod{}

	err := m.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if pod belongs to a PodSequence by looking for annotation
	podSeqName, hasPodSeq := pod.Annotations["podsequence.example.com/name"]
	if !hasPodSeq {
		// Not a PodSequence pod, skip
		return admission.Allowed("not a PodSequence pod")
	}

	groupIndexStr, hasGroup := pod.Annotations["podsequence.example.com/group"]
	if !hasGroup {
		return admission.Allowed("no group annotation")
	}

	var groupIndex int
	_, err = fmt.Sscanf(groupIndexStr, "%d", &groupIndex)
	if err != nil {
		log.Error(err, "Failed to parse group index", "groupIndex", groupIndexStr)
		return admission.Allowed("invalid group index")
	}

	// Fetch the PodSequence to get total groups
	podSeq := &schedulingv1alpha1.PodSequence{}
	if err := m.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      podSeqName,
	}, podSeq); err != nil {
		log.Error(err, "Failed to get PodSequence", "name", podSeqName)
		return admission.Allowed("PodSequence not found")
	}

	// Only inject tolerations for node-scoped sequences
	if podSeq.Spec.Scope != schedulingv1alpha1.PodSequenceScopeNode {
		return admission.Allowed("not node-scoped")
	}

	// Inject tolerations for all subsequent groups
	// Group 0 tolerates groups 1, 2, 3...
	// Group 1 tolerates groups 2, 3...
	// Group 2 tolerates nothing
	var tolerations []corev1.Toleration
	for i := groupIndex + 1; i < len(podSeq.Spec.PodGroups); i++ {
		taintKey := fmt.Sprintf("podsequence.example.com/%s-group-%d-blocked", podSeqName, i)
		tolerations = append(tolerations, corev1.Toleration{
			Key:      taintKey,
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		})
	}

	if len(tolerations) > 0 {
		// Append to existing tolerations
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, tolerations...)
		log.Info("Injected tolerations for PodSequence pod",
			"pod", pod.Name,
			"podSequence", podSeqName,
			"group", groupIndex,
			"tolerations", len(tolerations))
	}

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// InjectDecoder injects the decoder
func (m *PodSequenceMutator) InjectDecoder(d *admission.Decoder) error {
	m.decoder = d
	return nil
}
