package controller

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

// Ungate reasons, also used as metric label values.
const (
	ReasonSatisfied        = "satisfied"
	ReasonBypass           = "bypass"
	ReasonUnpinned         = "unpinned"
	ReasonNotParticipating = "node-not-participating"
	ReasonNodeMissing      = "node-missing"
	ReasonNoGroup          = "no-matching-group"
)

// NodePlanInput is everything needed to compute node-scoped actions for one PodSequence.
type NodePlanInput struct {
	Seq        *schedulingv1alpha1.PodSequence
	Nodes      []corev1.Node
	Pods       []corev1.Pod // pods in the target namespace
	DaemonSets []appsv1.DaemonSet
	Now        time.Time
}

// NodeState is the computed sequencing state of a node.
type NodeState struct {
	Name          string
	Participating bool
	// Satisfied is the number of consecutive groups (from 0) satisfied on the node.
	Satisfied int
	Message   string
	Failed    bool
}

// PodAction describes a gated pod the controller should release.
type PodAction struct {
	Pod            *corev1.Pod
	Group          int
	Reason         string
	AddAffinity    *corev1.NodeSelectorRequirement
	AddTolerations []corev1.Toleration
}

// TimedOutPod is a pinned pod gated for longer than spec.gateTimeout.
type TimedOutPod struct {
	Pod   *corev1.Pod
	Node  string
	Group int
}

// NodePlan is the output of ComputeNodePlan.
type NodePlan struct {
	Key       string
	Method    schedulingv1alpha1.NodeEligibilityMethod
	NumGroups int
	Nodes     []NodeState // every node, sorted by name
	Ungate    []PodAction
	// GatedPods counts member pods that remain gated after Ungate is applied.
	GatedPods int
	TimedOut  []TimedOutPod
	// MissingToleration lists DaemonSets that cannot run on tainted nodes (Taint mode only).
	MissingToleration []string
	// RequeueAfter is the earliest time-based re-evaluation (waitDuration/gateTimeout), 0 if none.
	RequeueAfter time.Duration
}

type member struct {
	pod   *corev1.Pod
	group int
	node  string
}

// ComputeNodePlan evaluates every participating node and every gated member pod.
func ComputeNodePlan(in NodePlanInput) (*NodePlan, error) {
	seq := in.Seq
	groups := seq.Spec.PodGroups
	numGroups := len(groups)
	gate := sequence.GateName(seq)
	plan := &NodePlan{
		Key:       sequence.EligibilityKey(seq.Namespace, seq.Name),
		Method:    sequence.EligibilityMethod(seq),
		NumGroups: numGroups,
	}

	nodeSel := labels.Everything()
	if seq.Spec.NodeSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(seq.Spec.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid nodeSelector: %w", err)
		}
		nodeSel = s
	}

	waits := make([]time.Duration, numGroups)
	dsByGroup := make([][]*appsv1.DaemonSet, numGroups)
	for i, g := range groups {
		w, err := sequence.WaitDuration(g)
		if err != nil {
			return nil, fmt.Errorf("invalid waitDuration in %s: %w", sequence.GroupName(g, i), err)
		}
		if _, err := sequence.GroupSelector(g); err != nil {
			return nil, fmt.Errorf("invalid podSelector in %s: %w", sequence.GroupName(g, i), err)
		}
		waits[i] = w
		dsByGroup[i] = sequence.DaemonSetsForGroup(in.DaemonSets, g)
	}

	if plan.Method == schedulingv1alpha1.NodeEligibilityTaint {
		probe := corev1.Taint{Key: plan.Key, Value: "0", Effect: corev1.TaintEffectNoSchedule}
		for _, dsList := range dsByGroup {
			for _, ds := range dsList {
				if !toleratesTaint(ds.Spec.Template.Spec.Tolerations, &probe) {
					plan.MissingToleration = append(plan.MissingToleration, ds.Name)
				}
			}
		}
	}

	targetNS := sequence.TargetNamespace(seq)
	var members []member
	podsByNodeGroup := map[string][][]*corev1.Pod{}
	for i := range in.Pods {
		pod := &in.Pods[i]
		if pod.Namespace != targetNS {
			continue
		}
		idx := sequence.GroupIndexForPod(seq, pod)
		if idx < 0 {
			if sequence.HasGate(pod, gate) && pod.Labels[sequence.SequenceLabel] == seq.Name && sequence.IsActive(pod) {
				// Opted in but matched by no group: never leave it stuck.
				plan.Ungate = append(plan.Ungate, PodAction{Pod: pod, Group: -1, Reason: ReasonNoGroup})
			}
			continue
		}
		if !sequence.IsActive(pod) {
			continue
		}
		m := member{pod: pod, group: idx, node: sequence.TargetNode(pod)}
		members = append(members, m)
		if m.node != "" {
			if podsByNodeGroup[m.node] == nil {
				podsByNodeGroup[m.node] = make([][]*corev1.Pod, numGroups)
			}
			podsByNodeGroup[m.node][idx] = append(podsByNodeGroup[m.node][idx], pod)
		}
	}

	nodes := append([]corev1.Node(nil), in.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	stateIdx := map[string]int{}
	for i := range nodes {
		node := &nodes[i]
		st := NodeState{Name: node.Name}
		st.Participating = node.DeletionTimestamp == nil && nodeSel.Matches(labels.Set(node.Labels))
		if st.Participating {
			st.Satisfied = numGroups
			for j := 0; j < numGroups; j++ {
				var podsHere []*corev1.Pod
				if byGroup := podsByNodeGroup[node.Name]; byGroup != nil {
					podsHere = byGroup[j]
				}
				ok, remaining, msg := groupSatisfiedOnNode(groups[j], j, node, podsHere, dsByGroup[j], waits[j], in.Now, plan.Key)
				plan.requeue(remaining)
				if !ok {
					st.Satisfied = j
					st.Message = msg
					break
				}
			}
		}
		stateIdx[node.Name] = len(plan.Nodes)
		plan.Nodes = append(plan.Nodes, st)
	}

	for _, m := range members {
		if !sequence.HasGate(m.pod, gate) {
			continue
		}
		if sequence.IsBypassed(m.pod) {
			plan.Ungate = append(plan.Ungate, PodAction{Pod: m.pod, Group: m.group, Reason: ReasonBypass})
			continue
		}
		if m.node == "" {
			plan.Ungate = append(plan.Ungate, unpinnedAction(plan, m))
			continue
		}
		si, exists := stateIdx[m.node]
		switch {
		case !exists:
			plan.Ungate = append(plan.Ungate, PodAction{Pod: m.pod, Group: m.group, Reason: ReasonNodeMissing})
		case !plan.Nodes[si].Participating:
			plan.Ungate = append(plan.Ungate, PodAction{Pod: m.pod, Group: m.group, Reason: ReasonNotParticipating})
		case plan.Nodes[si].Satisfied >= m.group:
			plan.Ungate = append(plan.Ungate, PodAction{Pod: m.pod, Group: m.group, Reason: ReasonSatisfied})
		default:
			plan.GatedPods++
			if seq.Spec.GateTimeout != nil && seq.Spec.GateTimeout.Duration > 0 {
				deadline := m.pod.CreationTimestamp.Add(seq.Spec.GateTimeout.Duration)
				if !in.Now.Before(deadline) {
					plan.Nodes[si].Failed = true
					plan.TimedOut = append(plan.TimedOut, TimedOutPod{Pod: m.pod, Node: m.node, Group: m.group})
				} else {
					plan.requeue(deadline.Sub(in.Now))
				}
			}
		}
	}
	return plan, nil
}

func unpinnedAction(plan *NodePlan, m member) PodAction {
	a := PodAction{Pod: m.pod, Group: m.group, Reason: ReasonUnpinned}
	switch plan.Method {
	case schedulingv1alpha1.NodeEligibilityTaint:
		a.AddTolerations = sequence.EligibilityTolerations(plan.Key, m.group, plan.NumGroups)
	default:
		if m.group >= 1 {
			req := sequence.EligibilityRequirement(plan.Key, m.group)
			a.AddAffinity = &req
		}
	}
	return a
}

// groupSatisfiedOnNode reports whether group j is satisfied on node, the remaining waitDuration
// (if that is the only thing missing) and a human readable reason when not satisfied.
func groupSatisfiedOnNode(g schedulingv1alpha1.PodGroup, j int, node *corev1.Node, pods []*corev1.Pod,
	daemonSets []*appsv1.DaemonSet, wait time.Duration, now time.Time, key string) (bool, time.Duration, string) {
	name := sequence.GroupName(g, j)
	for _, ds := range daemonSets {
		if !sequence.DaemonSetExpectedOnNode(ds, node, key) {
			continue
		}
		found := false
		for _, p := range pods {
			if sequence.OwnedBy(p, string(ds.UID)) {
				found = true
				break
			}
		}
		if !found {
			return false, 0, fmt.Sprintf("waiting for %s: DaemonSet %s has no pod on node yet", name, ds.Name)
		}
	}
	if len(pods) == 0 {
		if sequence.GroupPolicy(g) == schedulingv1alpha1.PodGroupPolicyRequireOnNode {
			return false, 0, fmt.Sprintf("waiting for %s: no pods on node (policy RequireOnNode)", name)
		}
		return true, 0, ""
	}
	var maxRemaining time.Duration
	for _, p := range pods {
		if !sequence.IsPodReady(p) {
			return false, 0, fmt.Sprintf("waiting for %s: pod %s not Ready", name, p.Name)
		}
		if rem := sequence.ReadyRemaining(p, wait, now); rem > maxRemaining {
			maxRemaining = rem
		}
	}
	if maxRemaining > 0 {
		return false, maxRemaining, fmt.Sprintf("waiting for %s: waitDuration (%s remaining)", name, maxRemaining.Round(time.Second))
	}
	return true, 0, ""
}

func (p *NodePlan) requeue(d time.Duration) {
	if d <= 0 {
		return
	}
	if p.RequeueAfter == 0 || d < p.RequeueAfter {
		p.RequeueAfter = d
	}
}

func toleratesTaint(tols []corev1.Toleration, taint *corev1.Taint) bool {
	for i := range tols {
		if tols[i].ToleratesTaint(taint) {
			return true
		}
	}
	return false
}
