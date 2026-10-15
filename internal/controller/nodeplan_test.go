package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

func mustPlan(t *testing.T, in NodePlanInput) *NodePlan {
	t.Helper()
	if in.Now.IsZero() {
		in.Now = t0.Add(time.Minute)
	}
	p, err := ComputeNodePlan(in)
	if err != nil {
		t.Fatalf("ComputeNodePlan: %v", err)
	}
	return p
}

func stateOf(t *testing.T, p *NodePlan, name string) NodeState {
	t.Helper()
	for _, s := range p.Nodes {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("node %s not in plan", name)
	return NodeState{}
}

func ungated(p *NodePlan) map[string]PodAction {
	m := map[string]PodAction{}
	for _, a := range p.Ungate {
		m[a.Pod.Name] = a
	}
	return m
}

// DS -> DS: each node progresses independently; a slow node does not block a ready one.
func TestPlanDaemonSetToDaemonSetPerNode(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	p := mustPlan(t, NodePlanInput{
		Seq:        nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels)),
		Nodes:      []corev1.Node{node("n1", nil), node("n2", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods: []corev1.Pod{
			dsPod("csi-1", csi, "n1", readyAt(t0)),
			dsPod("csi-2", csi, "n2", notReady()),
			dsPod("gpu-1", gpu, "n1", gated()),
			dsPod("gpu-2", gpu, "n2", gated()),
		},
	})
	if s := stateOf(t, p, "n1"); s.Satisfied != 1 {
		t.Fatalf("n1 satisfied = %d, want 1", s.Satisfied)
	}
	if s := stateOf(t, p, "n2"); s.Satisfied != 0 {
		t.Fatalf("n2 satisfied = %d, want 0", s.Satisfied)
	}
	u := ungated(p)
	if a, ok := u["gpu-1"]; !ok || a.Reason != ReasonSatisfied {
		t.Fatalf("gpu-1 should be ungated, got %+v", u)
	}
	if _, ok := u["gpu-2"]; ok {
		t.Fatal("gpu-2 must stay gated until csi-2 is Ready")
	}
	if p.GatedPods != 1 {
		t.Fatalf("GatedPods = %d, want 1", p.GatedPods)
	}
}

// A newly joined node where the DaemonSet pod has not been created yet must not count as satisfied.
func TestPlanExpectedDaemonSetPodMissing(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	p := mustPlan(t, NodePlanInput{
		Seq:        nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels)),
		Nodes:      []corev1.Node{node("new", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods:       []corev1.Pod{dsPod("gpu-new", gpu, "new", gated())},
	})
	if s := stateOf(t, p, "new"); s.Satisfied != 0 {
		t.Fatalf("satisfied = %d, want 0 (%s)", s.Satisfied, s.Message)
	}
	if len(p.Ungate) != 0 {
		t.Fatalf("nothing should be ungated, got %+v", ungated(p))
	}
}

// A DaemonSet that does not target a node (nodeSelector) is skipped there.
func TestPlanDaemonSetNotExpectedIsSkipped(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	csi.Spec.Template.Spec.NodeSelector = map[string]string{"storage": "true"}
	p := mustPlan(t, NodePlanInput{
		Seq:        nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels)),
		Nodes:      []corev1.Node{node("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods:       []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
	})
	if u := ungated(p); u["gpu-1"].Reason != ReasonSatisfied {
		t.Fatalf("gpu-1 should be ungated, got %+v", u)
	}
}

func TestPlanRequireOnNode(t *testing.T) {
	gpu := daemonSet("gpu", gpuLabels)
	g0 := group("csi", csiLabels)
	g0.Policy = schedulingv1alpha1.PodGroupPolicyRequireOnNode
	p := mustPlan(t, NodePlanInput{
		Seq:        nodeSeq(g0, group("gpu", gpuLabels)),
		Nodes:      []corev1.Node{node("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{gpu},
		Pods:       []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
	})
	if len(p.Ungate) != 0 {
		t.Fatal("RequireOnNode must block when the group has no pods on the node")
	}
}

func TestPlanWaitDuration(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	g0 := group("csi", csiLabels)
	g0.WaitDuration = "30s"
	in := NodePlanInput{
		Seq:        nodeSeq(g0, group("gpu", gpuLabels)),
		Nodes:      []corev1.Node{node("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods:       []corev1.Pod{dsPod("csi-1", csi, "n1", readyAt(t0)), dsPod("gpu-1", gpu, "n1", gated())},
		Now:        t0.Add(10 * time.Second),
	}
	p := mustPlan(t, in)
	if len(p.Ungate) != 0 {
		t.Fatal("must wait for waitDuration")
	}
	if p.RequeueAfter != 20*time.Second {
		t.Fatalf("RequeueAfter = %v, want 20s", p.RequeueAfter)
	}
	in.Now = t0.Add(31 * time.Second)
	if p := mustPlan(t, in); len(p.Ungate) != 1 {
		t.Fatal("should ungate after waitDuration")
	}
}

func TestPlanUnpinnedPodsNodeLabel(t *testing.T) {
	csi := daemonSet("csi", csiLabels)
	p := mustPlan(t, NodePlanInput{
		Seq:        nodeSeq(group("csi", csiLabels), group("web", appLabels)),
		Nodes:      []corev1.Node{node("n1", nil)},
		DaemonSets: []appsv1.DaemonSet{csi},
		Pods:       []corev1.Pod{plainPod("web-1", appLabels, gated())},
	})
	a, ok := ungated(p)["web-1"]
	if !ok || a.Reason != ReasonUnpinned {
		t.Fatalf("unpinned pod should be released with constraints, got %+v", p.Ungate)
	}
	if a.AddAffinity == nil || a.AddAffinity.Key != p.Key || a.AddAffinity.Operator != corev1.NodeSelectorOpGt || a.AddAffinity.Values[0] != "0" {
		t.Fatalf("unexpected affinity %+v", a.AddAffinity)
	}
	if len(a.AddTolerations) != 0 {
		t.Fatal("NodeLabel mode must not add tolerations")
	}
}

func TestPlanUnpinnedPodsTaint(t *testing.T) {
	csi := daemonSet("csi", csiLabels)
	seq := nodeSeq(group("csi", csiLabels), group("agent", gpuLabels), group("web", appLabels))
	seq.Spec.NodeEligibility = &schedulingv1alpha1.NodeEligibility{Method: schedulingv1alpha1.NodeEligibilityTaint}
	p := mustPlan(t, NodePlanInput{
		Seq: seq, Nodes: []corev1.Node{node("n1", nil)}, DaemonSets: []appsv1.DaemonSet{csi},
		Pods: []corev1.Pod{plainPod("web-1", appLabels, gated())},
	})
	a := ungated(p)["web-1"]
	if a.AddAffinity != nil || len(a.AddTolerations) != 1 || a.AddTolerations[0].Value != "2" {
		t.Fatalf("group 2 pod should tolerate only value 2, got %+v", a.AddTolerations)
	}
	if len(p.MissingToleration) != 1 || p.MissingToleration[0] != "csi" {
		t.Fatalf("csi DaemonSet lacks the taint toleration, got %v", p.MissingToleration)
	}
}

func TestPlanGateTimeout(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	seq := nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels))
	seq.Spec.GateTimeout = &metav1.Duration{Duration: 5 * time.Minute}
	in := NodePlanInput{
		Seq: seq, Nodes: []corev1.Node{node("n1", nil)}, DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods: []corev1.Pod{dsPod("csi-1", csi, "n1", notReady()), dsPod("gpu-1", gpu, "n1", gated(), createdAt(t0))},
		Now:  t0.Add(time.Minute),
	}
	p := mustPlan(t, in)
	if stateOf(t, p, "n1").Failed || p.RequeueAfter != 4*time.Minute {
		t.Fatalf("not timed out yet; RequeueAfter=%v", p.RequeueAfter)
	}
	in.Now = t0.Add(6 * time.Minute)
	p = mustPlan(t, in)
	if !stateOf(t, p, "n1").Failed || len(p.TimedOut) != 1 {
		t.Fatal("node should be marked Failed after gateTimeout")
	}
	if len(p.Ungate) != 0 || p.GatedPods != 1 {
		t.Fatal("pod must stay gated after gateTimeout")
	}
}

func TestPlanBypassNotParticipatingAndStray(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	seq := nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels))
	seq.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "gpu"}}
	p := mustPlan(t, NodePlanInput{
		Seq:        seq,
		Nodes:      []corev1.Node{node("gpu-node", map[string]string{"pool": "gpu"}), node("cpu-node", nil)},
		DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods: []corev1.Pod{
			dsPod("csi-1", csi, "gpu-node", notReady()),
			dsPod("gpu-bypass", gpu, "gpu-node", gated(), annotated(sequence.BypassAnnotation, "true")),
			dsPod("gpu-cpu", gpu, "cpu-node", gated()),
			plainPod("stray", map[string]string{"app": "other"}, gated(), labeled(sequence.SequenceLabel, "seq")),
		},
	})
	u := ungated(p)
	if u["gpu-bypass"].Reason != ReasonBypass || u["gpu-cpu"].Reason != ReasonNotParticipating || u["stray"].Reason != ReasonNoGroup {
		t.Fatalf("unexpected actions %+v", u)
	}
	if stateOf(t, p, "cpu-node").Participating {
		t.Fatal("cpu-node must not participate")
	}
}

func TestPlanInvalidWaitDuration(t *testing.T) {
	g := group("csi", csiLabels)
	g.WaitDuration = "soon"
	if _, err := ComputeNodePlan(NodePlanInput{Seq: nodeSeq(g)}); err == nil {
		t.Fatal("expected error")
	}
}

// A DaemonSet kept off the node only by the sequence's own taint or by boot-time lifecycle taints
// must hold its group unsatisfied rather than be skipped (which would release later groups early).
func TestPlanOwnAndTransientTaintsDoNotSkipDaemonSet(t *testing.T) {
	csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
	seq := nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels))
	seq.Spec.NodeEligibility = &schedulingv1alpha1.NodeEligibility{Method: schedulingv1alpha1.NodeEligibilityTaint}
	key := sequence.EligibilityKey(testNS, "seq")
	n := node("n1", nil,
		corev1.Taint{Key: key, Value: "0", Effect: corev1.TaintEffectNoSchedule},
		corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule})
	p := mustPlan(t, NodePlanInput{
		Seq: seq, Nodes: []corev1.Node{n}, DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods: []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
	})
	if s := stateOf(t, p, "n1"); s.Satisfied != 0 {
		t.Fatalf("satisfied = %d, want 0", s.Satisfied)
	}
	if len(p.Ungate) != 0 {
		t.Fatal("gpu-1 must stay gated while csi is expected but missing")
	}

	// A real, non-transient taint the DaemonSet doesn't tolerate still means "not expected here".
	n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}}
	p = mustPlan(t, NodePlanInput{
		Seq: seq, Nodes: []corev1.Node{n}, DaemonSets: []appsv1.DaemonSet{csi, gpu},
		Pods: []corev1.Pod{dsPod("gpu-1", gpu, "n1", gated())},
	})
	if s := stateOf(t, p, "n1"); s.Satisfied != 1 {
		t.Fatalf("satisfied = %d, want 1 (csi not expected on dedicated node)", s.Satisfied)
	}
}
