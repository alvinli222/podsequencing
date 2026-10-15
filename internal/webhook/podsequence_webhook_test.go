package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

func setup(t *testing.T, objs ...client.Object) (client.Client, *admission.Decoder) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = schedulingv1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(), admission.NewDecoder(s)
}

func request(t *testing.T, op admissionv1.Operation, obj interface{}) admission.Request {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "uid", Namespace: "default", Operation: op, Object: runtime.RawExtension{Raw: raw},
	}}
}

func seq(scope schedulingv1alpha1.PodSequenceScope, method schedulingv1alpha1.NodeEligibilityMethod) *schedulingv1alpha1.PodSequence {
	return &schedulingv1alpha1.PodSequence{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "seq"},
		Spec: schedulingv1alpha1.PodSequenceSpec{
			Scope:           scope,
			NodeEligibility: &schedulingv1alpha1.NodeEligibility{Method: method},
			PodGroups: []schedulingv1alpha1.PodGroup{
				{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "csi"}}},
			},
		},
	}
}

func hasPatch(resp admission.Response, pathPrefix string) bool {
	for _, p := range resp.Patches {
		if strings.HasPrefix(p.Path, pathPrefix) {
			return true
		}
	}
	return false
}

func TestPodGateInjector(t *testing.T) {
	c, dec := setup(t, seq(schedulingv1alpha1.PodSequenceScopeNode, schedulingv1alpha1.NodeEligibilityNodeLabel))
	h := &PodGateInjector{Client: c, Decoder: dec}

	member := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "csi", sequence.SequenceLabel: "seq"}}}
	resp := h.Handle(context.Background(), request(t, admissionv1.Create, member))
	if !resp.Allowed || !hasPatch(resp, "/spec/schedulingGates") {
		t.Fatalf("member pod should be gated: %+v", resp)
	}

	notOptedIn := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "csi"}}}
	if resp := h.Handle(context.Background(), request(t, admissionv1.Create, notOptedIn)); !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("pod without opt-in label must not be mutated: %+v", resp)
	}

	noGroup := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "x", sequence.SequenceLabel: "seq"}}}
	if resp := h.Handle(context.Background(), request(t, admissionv1.Create, noGroup)); len(resp.Patches) != 0 || len(resp.Warnings) == 0 {
		t.Fatalf("pod matching no group must not be gated and should warn: %+v", resp)
	}

	unknown := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "csi", sequence.SequenceLabel: "missing"}}}
	if resp := h.Handle(context.Background(), request(t, admissionv1.Create, unknown)); len(resp.Patches) != 0 {
		t.Fatal("pods referencing an unknown PodSequence must not be gated")
	}
}

func TestDaemonSetMutatorTaintMode(t *testing.T) {
	c, dec := setup(t, seq(schedulingv1alpha1.PodSequenceScopeNode, schedulingv1alpha1.NodeEligibilityTaint))
	h := &DaemonSetMutator{Client: c, Decoder: dec}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "csi", Labels: map[string]string{sequence.SequenceLabel: "seq"}},
		Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "csi"}}}},
	}
	resp := h.Handle(context.Background(), request(t, admissionv1.Create, ds))
	if !resp.Allowed || !hasPatch(resp, "/spec/template/spec/tolerations") || !hasPatch(resp, "/spec/template/metadata/labels") {
		t.Fatalf("expected toleration and template label patches: %+v", resp.Patches)
	}

	// Applying the mutation twice must be a no-op.
	if MutateDaemonSet(context.Background(), c, ds, "seq") != true {
		t.Fatal("first in-place mutation should change the object")
	}
	if MutateDaemonSet(context.Background(), c, ds, "seq") {
		t.Fatal("mutation must be idempotent")
	}
	if len(ds.Spec.Template.Spec.Tolerations) != 1 || ds.Spec.Template.Spec.Tolerations[0].Key != sequence.EligibilityKey("default", "seq") {
		t.Fatalf("unexpected tolerations %+v", ds.Spec.Template.Spec.Tolerations)
	}
}

func TestDaemonSetMutatorNodeLabelMode(t *testing.T) {
	c, dec := setup(t, seq(schedulingv1alpha1.PodSequenceScopeNode, schedulingv1alpha1.NodeEligibilityNodeLabel))
	h := &DaemonSetMutator{Client: c, Decoder: dec}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "csi", Labels: map[string]string{sequence.SequenceLabel: "seq"}},
		Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "csi", sequence.SequenceLabel: "seq"}}}},
	}
	if resp := h.Handle(context.Background(), request(t, admissionv1.Create, ds)); len(resp.Patches) != 0 {
		t.Fatalf("NodeLabel mode needs no DaemonSet changes, got %+v", resp.Patches)
	}
}

func TestPodGateInjectorSkipsBoundPods(t *testing.T) {
	c, dec := setup(t, seq(schedulingv1alpha1.PodSequenceScopeNode, schedulingv1alpha1.NodeEligibilityNodeLabel))
	h := &PodGateInjector{Client: c, Decoder: dec}
	bound := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "csi", sequence.SequenceLabel: "seq"}},
		Spec:       corev1.PodSpec{NodeName: "n1"},
	}
	if resp := h.Handle(context.Background(), request(t, admissionv1.Create, bound)); !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("pods with nodeName must not be gated: %+v", resp)
	}
}
