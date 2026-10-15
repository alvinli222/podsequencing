package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod.podsequence.example.com,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/mutate-apps-v1-daemonset,mutating=true,failurePolicy=ignore,sideEffects=None,groups=apps,resources=daemonsets,verbs=create;update,versions=v1,name=mdaemonset.podsequence.example.com,admissionReviewVersions=v1

// FindSequence returns the PodSequence named name that manages pods in namespace, preferring one
// that lives in that namespace. Returns nil if none exists.
func FindSequence(ctx context.Context, c client.Reader, namespace, name string) (*schedulingv1alpha1.PodSequence, error) {
	seq := &schedulingv1alpha1.PodSequence{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, seq)
	if err == nil && sequence.TargetNamespace(seq) == namespace && seq.DeletionTimestamp.IsZero() {
		return seq, nil
	}
	if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	list := &schedulingv1alpha1.PodSequenceList{}
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Name == name && sequence.TargetNamespace(s) == namespace && s.DeletionTimestamp.IsZero() {
			return s, nil
		}
	}
	return nil, nil
}

// PodGateInjector adds the PodSequence scheduling gate to pods that opt in with the
// podsequence.example.com/sequence label, so users never hard-code gates in pod templates.
type PodGateInjector struct {
	Client  client.Reader
	Decoder *admission.Decoder
}

// Handle processes admission requests
func (m *PodGateInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)
	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if pod.Namespace == "" {
		pod.Namespace = req.Namespace
	}
	if pod.Spec.NodeName != "" {
		// The API server rejects pods that set both nodeName and schedulingGates.
		return admission.Allowed("pod already bound to a node")
	}

	seqName := pod.Labels[sequence.SequenceLabel]
	if seqName == "" {
		return admission.Allowed("not a PodSequence pod")
	}
	if sequence.IsBypassed(pod) {
		return admission.Allowed("bypass annotation set")
	}
	seq, err := FindSequence(ctx, m.Client, pod.Namespace, seqName)
	if err != nil {
		logger.Error(err, "Failed to look up PodSequence", "name", seqName)
		return admission.Allowed("PodSequence lookup failed")
	}
	if seq == nil {
		return admission.Allowed("PodSequence not found").WithWarnings(
			"podsequence: no PodSequence " + seqName + " manages namespace " + pod.Namespace + "; pod not gated")
	}
	if sequence.GroupIndexForPod(seq, pod) < 0 {
		return admission.Allowed("pod matches no group").WithWarnings(
			"podsequence: pod matches no group of PodSequence " + seqName + "; pod not gated")
	}
	gate := sequence.GateName(seq)
	if sequence.HasGate(pod, gate) {
		return admission.Allowed("gate already present")
	}
	pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: gate})

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// DaemonSetMutator prepares DaemonSets that opt in with the podsequence.example.com/sequence label:
//   - copies the label into the pod template so the pod webhook gates the daemon pods;
//   - in Taint mode, adds a toleration for the sequence's node taint to the pod template. The
//     DaemonSet controller evaluates template tolerations before creating pods, so a pod-level
//     toleration would be too late.
type DaemonSetMutator struct {
	Client  client.Reader
	Decoder *admission.Decoder
}

// Handle processes admission requests
func (m *DaemonSetMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)
	ds := &appsv1.DaemonSet{}
	if err := m.Decoder.Decode(req, ds); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if ds.Namespace == "" {
		ds.Namespace = req.Namespace
	}
	seqName := ds.Labels[sequence.SequenceLabel]
	if seqName == "" {
		seqName = ds.Spec.Template.Labels[sequence.SequenceLabel]
	}
	if seqName == "" {
		return admission.Allowed("not a PodSequence DaemonSet")
	}

	if MutateDaemonSet(ctx, m.Client, ds, seqName) {
		logger.Info("Mutated DaemonSet for PodSequence", "daemonset", ds.Name, "podSequence", seqName)
	} else {
		return admission.Allowed("no changes")
	}
	marshaled, err := json.Marshal(ds)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// MutateDaemonSet applies the DaemonSet mutations in place. Returns true if ds changed.
func MutateDaemonSet(ctx context.Context, c client.Reader, ds *appsv1.DaemonSet, seqName string) bool {
	changed := false
	if ds.Spec.Template.Labels[sequence.SequenceLabel] != seqName {
		if ds.Spec.Template.Labels == nil {
			ds.Spec.Template.Labels = map[string]string{}
		}
		ds.Spec.Template.Labels[sequence.SequenceLabel] = seqName
		changed = true
	}
	seq, err := FindSequence(ctx, c, ds.Namespace, seqName)
	if err != nil || seq == nil {
		return changed
	}
	if sequence.Scope(seq) == schedulingv1alpha1.PodSequenceScopeNode &&
		sequence.EligibilityMethod(seq) == schedulingv1alpha1.NodeEligibilityTaint {
		key := sequence.EligibilityKey(seq.Namespace, seq.Name)
		if sequence.AddTolerations(&ds.Spec.Template.Spec, []corev1.Toleration{sequence.ExistsToleration(key)}) {
			changed = true
		}
	}
	return changed
}
