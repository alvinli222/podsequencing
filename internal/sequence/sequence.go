// Package sequence contains helpers shared by the PodSequence controller and webhooks.
package sequence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alvinli222/nodeseq"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
)

const (
	// Domain is the prefix used for all labels, annotations, taints and gates owned by this project.
	Domain = "podsequence.example.com/"

	// SequenceLabel opts a pod (or DaemonSet) into a PodSequence. The value is the PodSequence name.
	// The pod webhook only injects scheduling gates into pods carrying this label.
	SequenceLabel = Domain + "sequence"

	// BypassAnnotation, when "true" on a pod, makes the controller remove the gate immediately.
	BypassAnnotation = Domain + "bypass"

	// DefaultSchedulingGate is the gate name used when spec.schedulingGateName is empty.
	DefaultSchedulingGate = Domain + "sequence-gate"

	// EligibilityKeyPrefix prefixes the node label / taint key managed per node-scoped sequence.
	EligibilityKeyPrefix = Domain + "seq-"

	// MaxNodeStatusEntries caps status.nodeStatus.
	MaxNodeStatusEntries = 50

	// ReleasedLabelValue is written to an eligibility node label that no longer steers pods (sequence
	// deleted or switched to Taint mode) while unscheduled pods still require it, so that those pods'
	// "Gt k-1" affinity matches every node. The label is removed once no pending pod references it.
	ReleasedLabelValue = "999999"
)

// EligibilityKey returns the node label/taint key for a sequence. A hash keeps the key within the
// 63 character limit and unique across namespaces.
func EligibilityKey(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return EligibilityKeyPrefix + hex.EncodeToString(sum[:])[:10]
}

// IsEligibilityKey reports whether key was produced by EligibilityKey.
func IsEligibilityKey(key string) bool {
	return strings.HasPrefix(key, EligibilityKeyPrefix)
}

// IsLegacyGroupTaintKey matches taints created by earlier versions:
// podsequence.example.com/<name>-group-<N>-blocked
func IsLegacyGroupTaintKey(key string) bool {
	return strings.HasPrefix(key, Domain) && strings.Contains(key, "-group-") && strings.HasSuffix(key, "-blocked")
}

// TargetNamespace returns the namespace whose pods the sequence manages.
func TargetNamespace(seq *schedulingv1alpha1.PodSequence) string {
	if seq.Spec.Namespace != "" {
		return seq.Spec.Namespace
	}
	return seq.Namespace
}

// GateName returns the scheduling gate managed by the sequence.
func GateName(seq *schedulingv1alpha1.PodSequence) string {
	if seq.Spec.SchedulingGateName != "" {
		return seq.Spec.SchedulingGateName
	}
	return DefaultSchedulingGate
}

// Scope returns the effective scope.
func Scope(seq *schedulingv1alpha1.PodSequence) schedulingv1alpha1.PodSequenceScope {
	if seq.Spec.Scope == "" {
		return schedulingv1alpha1.PodSequenceScopeCluster
	}
	return seq.Spec.Scope
}

// EligibilityMethod returns the effective node eligibility method.
func EligibilityMethod(seq *schedulingv1alpha1.PodSequence) schedulingv1alpha1.NodeEligibilityMethod {
	if seq.Spec.NodeEligibility == nil || seq.Spec.NodeEligibility.Method == "" {
		return schedulingv1alpha1.NodeEligibilityNodeLabel
	}
	return seq.Spec.NodeEligibility.Method
}

// GroupPolicy returns the effective policy of a group.
func GroupPolicy(g schedulingv1alpha1.PodGroup) schedulingv1alpha1.PodGroupPolicy {
	if g.Policy == "" {
		return schedulingv1alpha1.PodGroupPolicySkipIfNotScheduled
	}
	return g.Policy
}

// GroupName returns a display name for the group at index i.
func GroupName(g schedulingv1alpha1.PodGroup, i int) string {
	if g.Name != "" {
		return g.Name
	}
	return fmt.Sprintf("Group %d", i+1)
}

// WaitDuration parses the group's waitDuration (empty means 0).
func WaitDuration(g schedulingv1alpha1.PodGroup) (time.Duration, error) {
	if g.WaitDuration == "" {
		return 0, nil
	}
	return time.ParseDuration(g.WaitDuration)
}

// GroupSelector returns the label selector of a group, or nil for name-based groups.
func GroupSelector(g schedulingv1alpha1.PodGroup) (labels.Selector, error) {
	if g.PodSelector == nil {
		return nil, nil
	}
	return metav1.LabelSelectorAsSelector(g.PodSelector)
}

// PodMatchesGroup reports whether the pod belongs to the group (by name or selector).
func PodMatchesGroup(pod *corev1.Pod, g schedulingv1alpha1.PodGroup) (bool, error) {
	if len(g.Pods) > 0 {
		for _, n := range g.Pods {
			if n == pod.Name {
				return true, nil
			}
		}
		return false, nil
	}
	sel, err := GroupSelector(g)
	if err != nil || sel == nil {
		return false, err
	}
	return sel.Matches(labels.Set(pod.Labels)), nil
}

// GroupIndexForPod returns the index of the first group the pod belongs to, or -1.
func GroupIndexForPod(seq *schedulingv1alpha1.PodSequence, pod *corev1.Pod) int {
	if pod.Namespace != "" && pod.Namespace != TargetNamespace(seq) {
		return -1
	}
	for i, g := range seq.Spec.PodGroups {
		if ok, err := PodMatchesGroup(pod, g); err == nil && ok {
			return i
		}
	}
	return -1
}

// IsBypassed reports whether the pod asks to skip sequencing.
func IsBypassed(pod *corev1.Pod) bool {
	return pod.Annotations[BypassAnnotation] == "true"
}

// EligibilityRequirement is the node affinity requirement added to an unpinned pod of group
// groupIndex (>=1) in NodeLabel mode: the node must have at least groupIndex satisfied groups.
func EligibilityRequirement(key string, groupIndex int) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{
		Key:      key,
		Operator: corev1.NodeSelectorOpGt,
		Values:   []string{strconv.Itoa(groupIndex - 1)},
	}
}

// EligibilityTolerations are the tolerations added to an unpinned pod of group groupIndex in Taint
// mode. The taint value is the number of satisfied groups, so the pod tolerates values >= groupIndex.
func EligibilityTolerations(key string, groupIndex, numGroups int) []corev1.Toleration {
	if groupIndex == 0 {
		return []corev1.Toleration{ExistsToleration(key)}
	}
	var tols []corev1.Toleration
	for v := groupIndex; v < numGroups; v++ {
		tols = append(tols, corev1.Toleration{
			Key:      key,
			Operator: corev1.TolerationOpEqual,
			Value:    strconv.Itoa(v),
			Effect:   corev1.TaintEffectNoSchedule,
		})
	}
	return tols
}

// ExistsToleration tolerates the eligibility taint regardless of value.
func ExistsToleration(key string) corev1.Toleration {
	return corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}
}

// AddTolerations appends missing tolerations. Returns true if the spec changed.
func AddTolerations(spec *corev1.PodSpec, tols []corev1.Toleration) bool {
	changed := false
	for i := range tols {
		found := false
		for _, existing := range spec.Tolerations {
			if existing.MatchToleration(&tols[i]) {
				found = true
				break
			}
		}
		if !found {
			spec.Tolerations = append(spec.Tolerations, tols[i])
			changed = true
		}
	}
	return changed
}

// AddRequiredNodeAffinity ANDs req into every required node selector term (creating one if none).
// This is the only form of node affinity mutation allowed on a scheduling-gated pod.
// Returns true if the spec changed.
func AddRequiredNodeAffinity(spec *corev1.PodSpec, req corev1.NodeSelectorRequirement) bool {
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		len(na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{req}}},
		}
		return true
	}
	changed := false
	terms := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i := range terms {
		if !hasRequirement(terms[i].MatchExpressions, req) {
			terms[i].MatchExpressions = append(terms[i].MatchExpressions, req)
			changed = true
		}
	}
	return changed
}

func hasRequirement(reqs []corev1.NodeSelectorRequirement, req corev1.NodeSelectorRequirement) bool {
	for _, r := range reqs {
		if r.Key == req.Key && r.Operator == req.Operator && strings.Join(r.Values, ",") == strings.Join(req.Values, ",") {
			return true
		}
	}
	return false
}

// PendingRequiredKeys returns the eligibility keys referenced by the required node affinity of
// active pods that are not yet scheduled.
func PendingRequiredKeys(pods []corev1.Pod) map[string]bool {
	keys := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" || !nodeseq.IsActive(p) || p.Spec.Affinity == nil || p.Spec.Affinity.NodeAffinity == nil ||
			p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			continue
		}
		for _, term := range p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, e := range term.MatchExpressions {
				if IsEligibilityKey(e.Key) {
					keys[e.Key] = true
				}
			}
		}
	}
	return keys
}
