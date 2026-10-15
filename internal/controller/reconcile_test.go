package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

type clock struct{ now time.Time }

func newReconciler(t *testing.T, clk *clock, objs ...client.Object) (*PodSequenceReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := schedulingv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&schedulingv1alpha1.PodSequence{}).Build()
	return &PodSequenceReconciler{
		Client: c, Scheme: s, Recorder: record.NewFakeRecorder(100), NodeScopeEnabled: true,
		Now: func() time.Time { return clk.now },
	}, c
}

func reconcileSeq(t *testing.T, r *PodSequenceReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: "seq"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getPod(t *testing.T, c client.Client, name string) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func getNode(t *testing.T, c client.Client, name string) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	return n
}

func getSeq(t *testing.T, c client.Client) *schedulingv1alpha1.PodSequence {
	t.Helper()
	s := &schedulingv1alpha1.PodSequence{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "seq"}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func setReady(t *testing.T, c client.Client, name string, ts time.Time) {
	t.Helper()
	p := getPod(t, c, name)
	readyAt(ts)(p)
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestReconcileNodeScopedNodeLabel(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	seq := nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels), group("web", appLabels))
	n1, n2 := node("n1", nil), node("n2", nil)
	pods := []corev1.Pod{
		dsPod("csi-1", csi, "n1", readyAt(t0)),
		dsPod("csi-2", csi, "n2", notReady()),
		dsPod("gpu-1", gpu, "n1", gated()),
		dsPod("gpu-2", gpu, "n2", gated()),
		plainPod("web-1", appLabels, gated()),
	}
	objs := []client.Object{seq, &n1, &n2, &csi, &gpu}
	for i := range pods {
		objs = append(objs, &pods[i])
	}
	clk := &clock{now: t0.Add(time.Minute)}
	r, c := newReconciler(t, clk, objs...)
	reconcileSeq(t, r)

	key := sequence.EligibilityKey(testNS, "seq")
	if v := getNode(t, c, "n1").Labels[key]; v != "1" {
		t.Fatalf("n1 label = %q, want 1", v)
	}
	if v := getNode(t, c, "n2").Labels[key]; v != "0" {
		t.Fatalf("n2 label = %q, want 0", v)
	}
	if sequence.HasGate(getPod(t, c, "gpu-1"), sequence.DefaultSchedulingGate) {
		t.Fatal("gpu-1 should be ungated")
	}
	if !sequence.HasGate(getPod(t, c, "gpu-2"), sequence.DefaultSchedulingGate) {
		t.Fatal("gpu-2 should stay gated")
	}
	web := getPod(t, c, "web-1")
	if sequence.HasGate(web, sequence.DefaultSchedulingGate) {
		t.Fatal("web-1 should be released with affinity")
	}
	expr := web.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if expr.Key != key || expr.Operator != corev1.NodeSelectorOpGt || expr.Values[0] != "1" {
		t.Fatalf("unexpected affinity %+v", expr)
	}

	st := getSeq(t, c).Status
	if st.Phase != schedulingv1alpha1.PodSequencePhaseInProgress || st.Summary == nil ||
		st.Summary.TotalNodes != 2 || st.Summary.SatisfiedNodes != 0 || st.Summary.GatedPods != 1 || st.NodeEligibilityKey != key {
		t.Fatalf("unexpected status %+v / %+v", st, st.Summary)
	}

	// Everything becomes Ready: both nodes reach all groups.
	setReady(t, c, "csi-2", t0)
	setReady(t, c, "gpu-1", t0)
	reconcileSeq(t, r)
	setReady(t, c, "gpu-2", t0)
	reconcileSeq(t, r)
	for _, n := range []string{"n1", "n2"} {
		if v := getNode(t, c, n).Labels[key]; v != "3" {
			t.Fatalf("%s label = %q, want 3", n, v)
		}
	}
	st = getSeq(t, c).Status
	if st.Phase != schedulingv1alpha1.PodSequencePhaseActive || len(st.NodeStatus) != 0 {
		t.Fatalf("expected Active with no pending nodes, got %s %+v", st.Phase, st.NodeStatus)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, schedulingv1alpha1.ConditionReady) {
		t.Fatal("Ready condition should be True")
	}
}

func TestReconcileNodeScopedTaint(t *testing.T) {
	csi := daemonSet("csi", csiLabels)
	key := sequence.EligibilityKey(testNS, "seq")
	csi.Spec.Template.Spec.Tolerations = []corev1.Toleration{sequence.ExistsToleration(key)}
	seq := nodeSeq(group("csi", csiLabels), group("web", appLabels))
	seq.Spec.NodeEligibility = &schedulingv1alpha1.NodeEligibility{Method: schedulingv1alpha1.NodeEligibilityTaint}
	other := corev1.Taint{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectPreferNoSchedule}
	n1 := node("n1", nil, other)
	csiPod := dsPod("csi-1", csi, "n1", notReady())
	clk := &clock{now: t0.Add(time.Minute)}
	r, c := newReconciler(t, clk, seq, &n1, &csi, &csiPod)

	reconcileSeq(t, r)
	taints := getNode(t, c, "n1").Spec.Taints
	if len(taints) != 2 || taints[0] != other || taints[1].Key != key || taints[1].Value != "0" {
		t.Fatalf("unexpected taints %+v", taints)
	}

	setReady(t, c, "csi-1", t0)
	reconcileSeq(t, r)
	taints = getNode(t, c, "n1").Spec.Taints
	if len(taints) != 1 || taints[0] != other {
		t.Fatalf("eligibility taint should be removed once all groups are satisfied, got %+v", taints)
	}
	if _, ok := getNode(t, c, "n1").Labels[key]; ok {
		t.Fatal("Taint mode must not label the node")
	}
}

func TestReconcileDeletionFailsOpen(t *testing.T) {
	gpu := daemonSet("gpu", gpuLabels)
	seq := nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels))
	seq.DeletionTimestamp = ptrTo(metav1.NewTime(t0))
	key := sequence.EligibilityKey(testNS, "seq")
	n1 := node("n1", map[string]string{key: "0", "keep": "me"},
		corev1.Taint{Key: key, Value: "0", Effect: corev1.TaintEffectNoSchedule})
	pod := dsPod("gpu-1", gpu, "n1", gated())
	r, c := newReconciler(t, &clock{now: t0}, seq, &n1, &gpu, &pod)

	reconcileSeq(t, r)
	if sequence.HasGate(getPod(t, c, "gpu-1"), sequence.DefaultSchedulingGate) {
		t.Fatal("gates must be removed on deletion")
	}
	n := getNode(t, c, "n1")
	if _, ok := n.Labels[key]; ok || n.Labels["keep"] != "me" || len(n.Spec.Taints) != 0 {
		t.Fatalf("node not cleaned correctly: %+v %+v", n.Labels, n.Spec.Taints)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "seq"}, &schedulingv1alpha1.PodSequence{}); err == nil {
		t.Fatal("PodSequence should be gone once the finalizer is removed")
	}
}

func TestReconcileNodeScopeDisabled(t *testing.T) {
	r, c := newReconciler(t, &clock{now: t0}, nodeSeq(group("csi", csiLabels)))
	r.NodeScopeEnabled = false
	reconcileSeq(t, r)
	if getSeq(t, c).Status.Phase != schedulingv1alpha1.PodSequencePhaseFailed {
		t.Fatal("node scope must fail in lite mode")
	}
}

func TestOrphanCleanup(t *testing.T) {
	live := nodeSeq(group("csi", csiLabels))
	liveKey := sequence.EligibilityKey(testNS, "seq")
	staleKey := sequence.EligibilityKey(testNS, "deleted")
	n1 := node("n1", map[string]string{liveKey: "1", staleKey: "0"},
		corev1.Taint{Key: "podsequence.example.com/old-group-1-blocked", Effect: corev1.TaintEffectNoSchedule},
		corev1.Taint{Key: staleKey, Value: "0", Effect: corev1.TaintEffectNoSchedule},
		corev1.Taint{Key: "keep", Effect: corev1.TaintEffectNoSchedule})
	r, c := newReconciler(t, &clock{now: t0}, live, &n1)
	if err := r.cleanupOrphanedNodeKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := getNode(t, c, "n1")
	if _, ok := n.Labels[staleKey]; ok || n.Labels[liveKey] != "1" {
		t.Fatalf("labels not cleaned correctly: %+v", n.Labels)
	}
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "keep" {
		t.Fatalf("taints not cleaned correctly: %+v", n.Spec.Taints)
	}
}

func clusterSeq(groups ...schedulingv1alpha1.PodGroup) *schedulingv1alpha1.PodSequence {
	s := nodeSeq(groups...)
	s.Spec.Scope = schedulingv1alpha1.PodSequenceScopeCluster
	s.Status.Phase = schedulingv1alpha1.PodSequencePhaseInProgress
	return s
}

func TestReconcileClusterWaitDuration(t *testing.T) {
	g0 := schedulingv1alpha1.PodGroup{Name: "db", Pods: []string{"db"}, WaitDuration: "30s"}
	g1 := schedulingv1alpha1.PodGroup{Name: "app", Pods: []string{"app"}}
	db := plainPod("db", nil, readyAt(t0))
	app := plainPod("app", nil, gated())
	clk := &clock{now: t0.Add(10 * time.Second)}
	r, c := newReconciler(t, clk, clusterSeq(g0, g1), &db, &app)

	res := reconcileSeq(t, r)
	if getSeq(t, c).Status.CurrentIndex != 0 || res.RequeueAfter != 20*time.Second {
		t.Fatalf("should wait 20s more, got index %d requeue %v", getSeq(t, c).Status.CurrentIndex, res.RequeueAfter)
	}
	// A second pass inside the window must keep waiting (previously this looped forever).
	clk.now = t0.Add(20 * time.Second)
	if res := reconcileSeq(t, r); res.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want 10s", res.RequeueAfter)
	}
	clk.now = t0.Add(31 * time.Second)
	reconcileSeq(t, r)
	if getSeq(t, c).Status.CurrentIndex != 1 {
		t.Fatal("should advance after waitDuration")
	}
	reconcileSeq(t, r)
	if sequence.HasGate(getPod(t, c, "app"), sequence.DefaultSchedulingGate) {
		t.Fatal("app should be ungated in the next group")
	}
}

func TestReconcileClusterWaitsForAllNamedPods(t *testing.T) {
	g0 := schedulingv1alpha1.PodGroup{Pods: []string{"a", "b"}}
	a := plainPod("a", nil, readyAt(t0))
	r, c := newReconciler(t, &clock{now: t0}, clusterSeq(g0), &a)
	reconcileSeq(t, r)
	if getSeq(t, c).Status.CurrentIndex != 0 {
		t.Fatal("must not advance while a named pod does not exist yet")
	}
}

func TestReconcileClusterReleasesRecreatedEarlierGroupPods(t *testing.T) {
	g0 := schedulingv1alpha1.PodGroup{PodSelector: &metav1.LabelSelector{MatchLabels: csiLabels}}
	g1 := schedulingv1alpha1.PodGroup{PodSelector: &metav1.LabelSelector{MatchLabels: appLabels}}
	seq := clusterSeq(g0, g1)
	seq.Status.CurrentIndex = 1
	recreated := plainPod("csi-new", csiLabels, gated())
	r, c := newReconciler(t, &clock{now: t0}, seq, &recreated)
	reconcileSeq(t, r)
	if sequence.HasGate(getPod(t, c, "csi-new"), sequence.DefaultSchedulingGate) {
		t.Fatal("recreated pods of a passed group must be released")
	}
}

func TestReconcileClusterCompletedReleasesLatePods(t *testing.T) {
	g0 := schedulingv1alpha1.PodGroup{PodSelector: &metav1.LabelSelector{MatchLabels: appLabels}}
	seq := clusterSeq(g0)
	seq.Status.CurrentIndex = 1
	seq.Status.Phase = schedulingv1alpha1.PodSequencePhaseCompleted
	late := plainPod("late", appLabels, gated())
	r, c := newReconciler(t, &clock{now: t0}, seq, &late)
	reconcileSeq(t, r)
	if sequence.HasGate(getPod(t, c, "late"), sequence.DefaultSchedulingGate) {
		t.Fatal("pods created after completion must not stay gated")
	}
}

// Deleting a NodeLabel sequence must not strand pods already released with the label affinity.
func TestReconcileDeletionKeepsLabelForPendingPods(t *testing.T) {
	seq := nodeSeq(group("csi", csiLabels), group("web", appLabels))
	seq.DeletionTimestamp = ptrTo(metav1.NewTime(t0))
	key := sequence.EligibilityKey(testNS, "seq")
	n1 := node("n1", map[string]string{key: "0"})
	pending := plainPod("web-1", appLabels)
	pending.Spec.Affinity = &corev1.Affinity{}
	sequence.AddRequiredNodeAffinity(&pending.Spec, sequence.EligibilityRequirement(key, 1))
	r, c := newReconciler(t, &clock{now: t0}, seq, &n1, &pending)

	reconcileSeq(t, r)
	if v := getNode(t, c, "n1").Labels[key]; v != sequence.ReleasedLabelValue {
		t.Fatalf("label = %q, want released value", v)
	}

	// Once the pod has scheduled, the orphan cleaner removes the label.
	p := getPod(t, c, "web-1")
	p.Spec.NodeName = "n1"
	if err := c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanupOrphanedNodeKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := getNode(t, c, "n1").Labels[key]; ok {
		t.Fatal("released label should be removed once no pending pod needs it")
	}
}

func TestOrphanCleanupIgnoresClusterScopedSequences(t *testing.T) {
	cs := clusterSeq(group("csi", csiLabels))
	key := sequence.EligibilityKey(testNS, "seq")
	n1 := node("n1", map[string]string{key: "1"}, corev1.Taint{Key: key, Value: "0", Effect: corev1.TaintEffectNoSchedule})
	r, c := newReconciler(t, &clock{now: t0}, cs, &n1)
	if err := r.cleanupOrphanedNodeKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := getNode(t, c, "n1")
	if _, ok := n.Labels[key]; ok || len(n.Spec.Taints) != 0 {
		t.Fatal("keys of a sequence that is no longer node-scoped must be cleaned up")
	}
}
