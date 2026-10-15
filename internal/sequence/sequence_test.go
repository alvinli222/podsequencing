package sequence

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
)

func dsAffinity(node string) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
		}}},
	}}
}

func TestTargetNode(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{"bound", corev1.Pod{Spec: corev1.PodSpec{NodeName: "n1"}}, "n1"},
		{"daemonset pinned", corev1.Pod{Spec: corev1.PodSpec{Affinity: dsAffinity("n2")}}, "n2"},
		{"no affinity", corev1.Pod{}, ""},
		{"label affinity only", corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}},
			}}},
		}}}}, ""},
		{"terms disagree", corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}},
				{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"b"}}}},
			}},
		}}}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TargetNode(&tt.pod); got != tt.want {
				t.Fatalf("TargetNode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEligibilityKey(t *testing.T) {
	k1 := EligibilityKey("ns", "a-very-long-podsequence-name-that-would-exceed-the-limit-of-sixty-three-chars")
	if k1 != EligibilityKey("ns", "a-very-long-podsequence-name-that-would-exceed-the-limit-of-sixty-three-chars") {
		t.Fatal("key not stable")
	}
	name := strings.TrimPrefix(k1, Domain)
	if len(name) > 63 {
		t.Fatalf("key name too long: %d", len(name))
	}
	if EligibilityKey("ns1", "x") == EligibilityKey("ns2", "x") {
		t.Fatal("keys must differ across namespaces")
	}
	if !IsEligibilityKey(k1) || IsLegacyGroupTaintKey(k1) {
		t.Fatal("key classification wrong")
	}
	if !IsLegacyGroupTaintKey("podsequence.example.com/foo-group-1-blocked") {
		t.Fatal("legacy key not detected")
	}
}

func TestAddRequiredNodeAffinity(t *testing.T) {
	req := EligibilityRequirement("podsequence.example.com/seq-abc", 2)
	if req.Operator != corev1.NodeSelectorOpGt || req.Values[0] != "1" {
		t.Fatalf("unexpected requirement %+v", req)
	}

	spec := &corev1.PodSpec{}
	if !AddRequiredNodeAffinity(spec, req) {
		t.Fatal("expected change on empty spec")
	}
	if AddRequiredNodeAffinity(spec, req) {
		t.Fatal("expected idempotent")
	}

	spec = &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "a", Operator: corev1.NodeSelectorOpExists}}},
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "b", Operator: corev1.NodeSelectorOpExists}}},
		}},
	}}}
	AddRequiredNodeAffinity(spec, req)
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("terms must not be added, got %d", len(terms))
	}
	for i, term := range terms {
		if len(term.MatchExpressions) != 2 || term.MatchExpressions[1].Key != req.Key {
			t.Fatalf("term %d not ANDed with requirement: %+v", i, term)
		}
	}
}

func TestEligibilityTolerations(t *testing.T) {
	key := "podsequence.example.com/seq-abc"
	if tols := EligibilityTolerations(key, 0, 3); len(tols) != 1 || tols[0].Operator != corev1.TolerationOpExists {
		t.Fatalf("group 0 should tolerate any value: %+v", tols)
	}
	tols := EligibilityTolerations(key, 1, 3)
	if len(tols) != 2 || tols[0].Value != "1" || tols[1].Value != "2" {
		t.Fatalf("unexpected tolerations %+v", tols)
	}
	spec := &corev1.PodSpec{}
	if !AddTolerations(spec, tols) || AddTolerations(spec, tols) {
		t.Fatal("AddTolerations should add once")
	}
	taint0 := corev1.Taint{Key: key, Value: "0", Effect: corev1.TaintEffectNoSchedule}
	taint1 := corev1.Taint{Key: key, Value: "1", Effect: corev1.TaintEffectNoSchedule}
	tolerates := func(taint *corev1.Taint) bool {
		for i := range spec.Tolerations {
			if spec.Tolerations[i].ToleratesTaint(taint) {
				return true
			}
		}
		return false
	}
	if tolerates(&taint0) || !tolerates(&taint1) {
		t.Fatal("group 1 pod must tolerate value 1 but not 0")
	}
}

func TestDaemonSetShouldRunOnNode(t *testing.T) {
	ds := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{NodeSelector: map[string]string{"pool": "gpu"}},
	}}}
	gpu := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Labels: map[string]string{"pool": "gpu"}}}
	cpu := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cpu", Labels: map[string]string{"pool": "cpu"}}}
	if !DaemonSetShouldRunOnNode(ds, gpu) || DaemonSetShouldRunOnNode(ds, cpu) {
		t.Fatal("nodeSelector not honored")
	}

	tainted := gpu.DeepCopy()
	tainted.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule}}
	if DaemonSetShouldRunOnNode(ds, tainted) {
		t.Fatal("untolerated NoSchedule taint should block")
	}
	tainted.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}}
	if !DaemonSetShouldRunOnNode(ds, tainted) {
		t.Fatal("DaemonSet default tolerations should apply")
	}
	tainted.Spec.Taints = []corev1.Taint{{Key: "x", Effect: corev1.TaintEffectPreferNoSchedule}}
	if !DaemonSetShouldRunOnNode(ds, tainted) {
		t.Fatal("PreferNoSchedule should not block")
	}
}

func TestGroupIndexForPod(t *testing.T) {
	seq := &schedulingv1alpha1.PodSequence{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"},
		Spec: schedulingv1alpha1.PodSequenceSpec{PodGroups: []schedulingv1alpha1.PodGroup{
			{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}}},
			{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}}},
		}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{"app": "b"}}}
	if GroupIndexForPod(seq, pod) != 1 {
		t.Fatal("expected group 1")
	}
	pod.Namespace = "other"
	if GroupIndexForPod(seq, pod) != -1 {
		t.Fatal("pods in other namespaces must not match")
	}
}
