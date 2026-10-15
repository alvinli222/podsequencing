package sequence

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1helper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
)

// Tolerations the DaemonSet controller adds to every daemon pod
// (see k8s.io/kubernetes/pkg/controller/daemon/util.AddOrUpdateDaemonPodTolerations).
var daemonSetDefaultTolerations = []corev1.Toleration{
	{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeDiskPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeMemoryPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodePIDPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeUnschedulable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
}

// DaemonSetShouldRunOnNode emulates the DaemonSet controller's decision of whether a new daemon pod
// should be created on node: node name, required node affinity / nodeSelector, and taints versus the
// template tolerations plus the DaemonSet controller's default tolerations.
func DaemonSetShouldRunOnNode(ds *appsv1.DaemonSet, node *corev1.Node) bool {
	spec := ds.Spec.Template.Spec
	if spec.NodeName != "" && spec.NodeName != node.Name {
		return false
	}
	pod := &corev1.Pod{Spec: *spec.DeepCopy()}
	if ok, err := nodeaffinity.GetRequiredNodeAffinity(pod).Match(node); err != nil || !ok {
		return false
	}
	tols := append([]corev1.Toleration{}, spec.Tolerations...)
	tols = append(tols, daemonSetDefaultTolerations...)
	if spec.HostNetwork {
		tols = append(tols, corev1.Toleration{
			Key: corev1.TaintNodeNetworkUnavailable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	_, untolerated := v1helper.FindMatchingUntoleratedTaint(node.Spec.Taints, tols, func(t *corev1.Taint) bool {
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	})
	return !untolerated
}

// transientTaintKeys are node lifecycle taints that disappear once a node finishes booting.
var transientTaintKeys = map[string]bool{
	corev1.TaintNodeNotReady:                        true,
	corev1.TaintNodeUnreachable:                     true,
	corev1.TaintNodeNetworkUnavailable:              true,
	"node.cloudprovider.kubernetes.io/uninitialized": true,
}

// DaemonSetExpectedOnNode reports whether the DaemonSet is expected to (eventually) run a pod on the
// node. It is DaemonSetShouldRunOnNode evaluated without transient node lifecycle taints and without
// the sequence's own eligibility taint (ownKey), so a DaemonSet that is only temporarily kept off the
// node keeps its group unsatisfied instead of being skipped.
func DaemonSetExpectedOnNode(ds *appsv1.DaemonSet, node *corev1.Node, ownKey string) bool {
	filtered := make([]corev1.Taint, 0, len(node.Spec.Taints))
	for _, t := range node.Spec.Taints {
		if t.Key == ownKey || transientTaintKeys[t.Key] {
			continue
		}
		filtered = append(filtered, t)
	}
	n := *node
	n.Spec.Taints = filtered
	return DaemonSetShouldRunOnNode(ds, &n)
}

// DaemonSetsForGroup returns the DaemonSets whose pod template is selected by the group.
func DaemonSetsForGroup(dsList []appsv1.DaemonSet, g schedulingv1alpha1.PodGroup) []*appsv1.DaemonSet {
	sel, err := GroupSelector(g)
	if err != nil || sel == nil {
		return nil
	}
	var out []*appsv1.DaemonSet
	for i := range dsList {
		ds := &dsList[i]
		if ds.DeletionTimestamp != nil {
			continue
		}
		if sel.Matches(labels.Set(ds.Spec.Template.Labels)) {
			out = append(out, ds)
		}
	}
	return out
}

// OwnedBy reports whether the pod's controller owner has the given UID.
func OwnedBy(pod *corev1.Pod, uid string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && string(ref.UID) == uid {
			return true
		}
	}
	return false
}
