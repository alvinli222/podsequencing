package controller

import (
	"fmt"
	"time"

	"github.com/alvinli222/nodeseq"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

// Ungate reasons, also used as metric label values.
const (
	ReasonSatisfied        = nodeseq.ReasonSatisfied
	ReasonBypass           = nodeseq.ReasonBypass
	ReasonUnpinned         = nodeseq.ReasonUnpinned
	ReasonNotParticipating = nodeseq.ReasonNotParticipating
	ReasonNodeMissing      = nodeseq.ReasonNodeMissing
	ReasonNoGroup          = "no-matching-group"
)

// NodeState is the computed sequencing state of a node.
type NodeState = nodeseq.NodeState

// TimedOutPod is a pinned pod gated for longer than spec.gateTimeout.
type TimedOutPod = nodeseq.TimedOut

// NodePlanInput is everything needed to compute node-scoped actions for one PodSequence.
type NodePlanInput struct {
	Seq        *schedulingv1alpha1.PodSequence
	Nodes      []corev1.Node
	Pods       []corev1.Pod // pods in the target namespace
	DaemonSets []appsv1.DaemonSet
	Now        time.Time
}

// PodAction describes a gated pod the controller should release.
type PodAction struct {
	Pod            *corev1.Pod
	Group          int
	Reason         string
	AddAffinity    *corev1.NodeSelectorRequirement
	AddTolerations []corev1.Toleration
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

// ComputeNodePlan adapts a node-scoped PodSequence to nodeseq.Compute and adds the PodSequence
// specific parts: node label / taint constraints for unpinned pods, release of opted-in pods that match
// no group, and the Taint-mode toleration check.
func ComputeNodePlan(in NodePlanInput) (*NodePlan, error) {
	seq := in.Seq
	groups, err := nodeseqGroups(seq)
	if err != nil {
		return nil, err
	}
	var nodeSel labels.Selector
	if seq.Spec.NodeSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(seq.Spec.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid nodeSelector: %w", err)
		}
		nodeSel = s
	}
	var gateTimeout time.Duration
	if seq.Spec.GateTimeout != nil {
		gateTimeout = seq.Spec.GateTimeout.Duration
	}

	key := sequence.EligibilityKey(seq.Namespace, seq.Name)
	gate := sequence.GateName(seq)
	core, err := nodeseq.Compute(nodeseq.Input{
		Groups:          groups,
		Namespace:       sequence.TargetNamespace(seq),
		Gate:            gate,
		Nodes:           in.Nodes,
		NodeSelector:    nodeSel,
		Pods:            in.Pods,
		DaemonSets:      in.DaemonSets,
		Now:             in.Now,
		GateTimeout:     gateTimeout,
		IgnoreTaintKeys: []string{key},
		Bypass:          sequence.IsBypassed,
	})
	if err != nil {
		return nil, err
	}

	plan := &NodePlan{
		Key:          key,
		Method:       sequence.EligibilityMethod(seq),
		NumGroups:    core.NumGroups,
		Nodes:        core.Nodes,
		GatedPods:    core.GatedPods,
		TimedOut:     core.TimedOut,
		RequeueAfter: core.RequeueAfter,
	}

	for _, r := range core.Release {
		a := PodAction{Pod: r.Pod, Group: r.Group, Reason: r.Reason}
		if r.Reason == nodeseq.ReasonUnpinned {
			addEligibilityConstraints(plan, &a)
		}
		plan.Ungate = append(plan.Ungate, a)
	}

	// Opted in via the sequence label but matched by no group: never leave it stuck.
	targetNS := sequence.TargetNamespace(seq)
	for i := range in.Pods {
		pod := &in.Pods[i]
		if pod.Namespace == targetNS && pod.Labels[sequence.SequenceLabel] == seq.Name &&
			nodeseq.GroupIndex(groups, pod) < 0 && nodeseq.HasGate(pod, gate) && nodeseq.IsActive(pod) {
			plan.Ungate = append(plan.Ungate, PodAction{Pod: pod, Group: -1, Reason: ReasonNoGroup})
		}
	}

	if plan.Method == schedulingv1alpha1.NodeEligibilityTaint {
		probe := corev1.Taint{Key: key, Value: "0", Effect: corev1.TaintEffectNoSchedule}
		for _, dsList := range core.DaemonSets {
			for _, ds := range dsList {
				if !toleratesTaint(ds.Spec.Template.Spec.Tolerations, &probe) {
					plan.MissingToleration = append(plan.MissingToleration, ds.Name)
				}
			}
		}
	}
	return plan, nil
}

// nodeseqGroups converts the sequence's groups (node scope requires podSelector on every group).
func nodeseqGroups(seq *schedulingv1alpha1.PodSequence) ([]nodeseq.Group, error) {
	groups := make([]nodeseq.Group, 0, len(seq.Spec.PodGroups))
	for i, g := range seq.Spec.PodGroups {
		name := sequence.GroupName(g, i)
		wait, err := sequence.WaitDuration(g)
		if err != nil {
			return nil, fmt.Errorf("invalid waitDuration in %s: %w", name, err)
		}
		sel, err := sequence.GroupSelector(g)
		if err != nil {
			return nil, fmt.Errorf("invalid podSelector in %s: %w", name, err)
		}
		if sel == nil {
			return nil, fmt.Errorf("%s: node-scoped sequences require podSelector", name)
		}
		groups = append(groups, nodeseq.Group{
			Name:     name,
			Selector: sel,
			Wait:     wait,
			Policy:   nodeseq.Policy(sequence.GroupPolicy(g)),
		})
	}
	return groups, nil
}

// addEligibilityConstraints steers an unpinned pod of group k to nodes with at least k satisfied groups.
func addEligibilityConstraints(plan *NodePlan, a *PodAction) {
	switch plan.Method {
	case schedulingv1alpha1.NodeEligibilityTaint:
		a.AddTolerations = sequence.EligibilityTolerations(plan.Key, a.Group, plan.NumGroups)
	default:
		if a.Group >= 1 {
			req := sequence.EligibilityRequirement(plan.Key, a.Group)
			a.AddAffinity = &req
		}
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
