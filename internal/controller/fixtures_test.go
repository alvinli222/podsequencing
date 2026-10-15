package controller

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

const testNS = "default"

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func node(name string, lbls map[string]string, taints ...corev1.Taint) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}, Spec: corev1.NodeSpec{Taints: taints}}
}

func daemonSet(name string, lbls map[string]string) appsv1.DaemonSet {
	return appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, UID: types.UID("uid-" + name)},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbls}},
		},
	}
}

type podOpt func(*corev1.Pod)

func gated() podOpt {
	return func(p *corev1.Pod) {
		p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: sequence.DefaultSchedulingGate}}
	}
}

func readyAt(ts time.Time) podOpt {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(ts)}}
	}
}

func notReady() podOpt {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	}
}

func createdAt(ts time.Time) podOpt {
	return func(p *corev1.Pod) { p.CreationTimestamp = metav1.NewTime(ts) }
}

func annotated(k, v string) podOpt {
	return func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[k] = v
	}
}

func labeled(k, v string) podOpt {
	return func(p *corev1.Pod) { p.Labels[k] = v }
}

// dsPod builds a pod as the DaemonSet controller would: owned by ds and pinned to nodeName.
func dsPod(name string, ds appsv1.DaemonSet, nodeName string, opts ...podOpt) corev1.Pod {
	lbls := map[string]string{}
	for k, v := range ds.Spec.Template.Labels {
		lbls[k] = v
	}
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: name, Labels: lbls, CreationTimestamp: metav1.NewTime(t0),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: boolPtr(true)}},
		},
		Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{nodeName}}},
			}}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// plainPod builds an unpinned pod (e.g. from a Deployment).
func plainPod(name string, lbls map[string]string, opts ...podOpt) corev1.Pod {
	l := map[string]string{}
	for k, v := range lbls {
		l[k] = v
	}
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, Labels: l, CreationTimestamp: metav1.NewTime(t0)},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func nodeSeq(groups ...schedulingv1alpha1.PodGroup) *schedulingv1alpha1.PodSequence {
	return &schedulingv1alpha1.PodSequence{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "seq", Finalizers: []string{FinalizerKey}},
		Spec:       schedulingv1alpha1.PodSequenceSpec{Scope: schedulingv1alpha1.PodSequenceScopeNode, PodGroups: groups},
	}
}

func group(name string, lbls map[string]string) schedulingv1alpha1.PodGroup {
	return schedulingv1alpha1.PodGroup{Name: name, PodSelector: &metav1.LabelSelector{MatchLabels: lbls}}
}

var (
	csiLabels = map[string]string{"app": "csi"}
	gpuLabels = map[string]string{"app": "gpu-agent"}
	appLabels = map[string]string{"app": "web"}
)

func boolPtr(b bool) *bool { return &b }
