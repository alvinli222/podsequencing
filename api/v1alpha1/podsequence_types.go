package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodGroupPolicy controls how a group is evaluated on a node that has no pods of the group.
// Only used for node-scoped sequences.
// +kubebuilder:validation:Enum=SkipIfNotScheduled;RequireOnNode
type PodGroupPolicy string

const (
	// PodGroupPolicySkipIfNotScheduled treats the group as satisfied on a node when no pod of the
	// group targets the node and no DaemonSet backing the group is expected to run there.
	PodGroupPolicySkipIfNotScheduled PodGroupPolicy = "SkipIfNotScheduled"
	// PodGroupPolicyRequireOnNode requires at least one Ready pod of the group on the node.
	PodGroupPolicyRequireOnNode PodGroupPolicy = "RequireOnNode"
)

// PodGroup represents a group of pods that should start together
// +kubebuilder:validation:XValidation:rule="(has(self.pods) && size(self.pods) > 0) != has(self.podSelector)",message="exactly one of pods or podSelector must be specified"
type PodGroup struct {
	// Name is an optional name for the pod group (for identification purposes)
	// +optional
	Name string `json:"name,omitempty"`

	// Pods is the list of specific pod names in this group
	// Use this for explicitly named pods
	// Either Pods or PodSelector must be specified, but not both
	// +optional
	Pods []string `json:"pods,omitempty"`

	// PodSelector selects pods using label selector (useful for DaemonSets, StatefulSets, etc.)
	// All pods matching the selector will be included in this group
	// Either Pods or PodSelector must be specified, but not both
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// WaitDuration specifies how long to wait after all pods in this group are ready
	// before allowing the next group to start. Useful for initialization, stabilization, or health check time.
	// For node-scoped sequences this is evaluated per node.
	// Format: Go duration (e.g., "30s", "5m", "1h30m")
	// If not specified or "0s", no additional wait is applied
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))*$`
	WaitDuration string `json:"waitDuration,omitempty"`

	// Policy controls how this group is evaluated on nodes without pods of this group
	// (node-scoped sequences only). Defaults to SkipIfNotScheduled.
	// +kubebuilder:default=SkipIfNotScheduled
	// +optional
	Policy PodGroupPolicy `json:"policy,omitempty"`
}

// PodSequenceScope defines the scope at which pod sequencing is enforced
type PodSequenceScope string

const (
	// PodSequenceScopeCluster enforces sequencing across the entire cluster
	// All pods in a group must be ready before any pods in the next group start
	PodSequenceScopeCluster PodSequenceScope = "Cluster"
	// PodSequenceScopeNode enforces sequencing per-node
	// Pods in a group on each node must be ready before the next group's pods on that node start
	PodSequenceScopeNode PodSequenceScope = "Node"
)

// NodeEligibilityMethod defines how unpinned pods (pods whose node is not known before
// scheduling, e.g. Deployment pods) are kept off nodes whose earlier groups are not ready.
// +kubebuilder:validation:Enum=NodeLabel;Taint
type NodeEligibilityMethod string

const (
	// NodeEligibilityNodeLabel labels each participating node with the number of satisfied groups
	// and adds a required node affinity to unpinned member pods. Only member pods are affected.
	NodeEligibilityNodeLabel NodeEligibilityMethod = "NodeLabel"
	// NodeEligibilityTaint taints each participating node with NoSchedule until every group is
	// satisfied on it. All pods that do not tolerate the taint (including non-members) are blocked.
	NodeEligibilityTaint NodeEligibilityMethod = "Taint"
)

// NodeEligibility configures how unpinned pods are steered to nodes that are ready for them
type NodeEligibility struct {
	// Method is either NodeLabel (default) or Taint
	// +kubebuilder:default=NodeLabel
	// +optional
	Method NodeEligibilityMethod `json:"method,omitempty"`
}

// PodSequenceSpec defines the desired state of PodSequence
// +kubebuilder:validation:XValidation:rule="!has(self.scope) || self.scope != 'Node' || self.podGroups.all(g, has(g.podSelector))",message="node-scoped sequences require podSelector on every group"
// +kubebuilder:validation:XValidation:rule="(has(self.scope) ? self.scope : 'Cluster') == (has(oldSelf.scope) ? oldSelf.scope : 'Cluster')",message="scope is immutable"
type PodSequenceSpec struct {
	// PodGroups is an ordered list of pod groups that should be scheduled sequentially
	// Each group contains one or more pods. All pods in a group will have their scheduling
	// gates removed together, and the next group will only start after all pods in the
	// current group are ready
	// +kubebuilder:validation:MinItems=1
	PodGroups []PodGroup `json:"podGroups"`

	// Scope determines whether sequencing is enforced at the cluster or node level
	// - Cluster (default): All pods in a group across the cluster must be ready before the next group starts
	// - Node: Pods in a group on each node must be ready before the next group's pods on that node start
	// Immutable.
	// +kubebuilder:validation:Enum=Cluster;Node
	// +kubebuilder:default=Cluster
	// +optional
	Scope PodSequenceScope `json:"scope,omitempty"`

	// Namespace is the namespace where the pods are located
	// If not specified, defaults to the namespace of the PodSequence resource
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// SchedulingGateName is the name of the scheduling gate to manage
	// Defaults to "podsequence.example.com/sequence-gate"
	// +optional
	SchedulingGateName string `json:"schedulingGateName,omitempty"`

	// NodeSelector restricts which nodes participate in a node-scoped sequence.
	// Defaults to all nodes.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// NodeEligibility configures how unpinned pods (e.g. Deployment pods) are kept off nodes
	// whose earlier groups are not ready. Node-scoped sequences only.
	// +optional
	NodeEligibility *NodeEligibility `json:"nodeEligibility,omitempty"`

	// GateTimeout is how long a pinned pod (e.g. a DaemonSet pod) may stay gated waiting for
	// earlier groups on its node. When exceeded, the node is marked Failed in status and a
	// Warning event is emitted; the pod stays gated. Node-scoped sequences only.
	// +optional
	GateTimeout *metav1.Duration `json:"gateTimeout,omitempty"`
}

// NodeSequenceStatus tracks the sequencing status for a specific node
type NodeSequenceStatus struct {
	// NodeName is the name of the node
	NodeName string `json:"nodeName"`

	// CurrentIndex is the number of consecutive groups (from the first) that are satisfied on this node.
	// It is also the index of the group currently being waited on.
	CurrentIndex int `json:"currentIndex"`

	// Phase indicates the current phase for this node (InProgress or Failed)
	Phase PodSequencePhase `json:"phase"`

	// Message provides details about what the node is waiting on
	// +optional
	Message string `json:"message,omitempty"`
}

// NodeSummary aggregates per-node progress for node-scoped sequences
type NodeSummary struct {
	// TotalNodes is the number of participating nodes
	TotalNodes int `json:"totalNodes"`
	// SatisfiedNodes is the number of participating nodes on which every group is satisfied
	SatisfiedNodes int `json:"satisfiedNodes"`
	// FailedNodes is the number of nodes with a pod gated longer than gateTimeout
	FailedNodes int `json:"failedNodes"`
	// GatedPods is the number of member pods still holding the scheduling gate
	GatedPods int `json:"gatedPods"`
}

// PodSequenceStatus defines the observed state of PodSequence
type PodSequenceStatus struct {
	// CurrentIndex is the index in the sequence that is currently being processed
	// For cluster-scoped sequences, this is the cluster-wide group index
	// For node-scoped sequences, this is the minimum index across participating nodes
	CurrentIndex int `json:"currentIndex"`

	// Phase indicates the current phase of the sequence
	// Possible values: Pending, InProgress, Active, Completed, Failed
	Phase PodSequencePhase `json:"phase"`

	// NodeStatus lists participating nodes that are not yet fully satisfied (node-scoped only).
	// Capped at 50 entries; see Summary for totals.
	// +optional
	NodeStatus []NodeSequenceStatus `json:"nodeStatus,omitempty"`

	// Summary aggregates per-node progress (node-scoped only)
	// +optional
	Summary *NodeSummary `json:"summary,omitempty"`

	// NodeEligibilityKey is the node label or taint key managed for this sequence (node-scoped only)
	// +optional
	NodeEligibilityKey string `json:"nodeEligibilityKey,omitempty"`

	// Conditions represent the latest available observations of the PodSequence's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProcessedPods is a list of pod names that have been successfully processed
	ProcessedPods []string `json:"processedPods,omitempty"`

	// CurrentGroupPods is a list of pod names in the current group being processed
	// +optional
	CurrentGroupPods []string `json:"currentGroupPods,omitempty"`

	// ReadyPodsInCurrentGroup is the count of ready pods in the current group
	// +optional
	ReadyPodsInCurrentGroup int `json:"readyPodsInCurrentGroup,omitempty"`

	// Message provides additional information about the current state
	Message string `json:"message,omitempty"`
}

type PodSequencePhase string

const (
	// PodSequencePhasePending indicates the sequence has not started yet
	PodSequencePhasePending PodSequencePhase = "Pending"
	// PodSequencePhaseInProgress indicates the sequence is currently being processed
	PodSequencePhaseInProgress PodSequencePhase = "InProgress"
	// PodSequencePhaseActive indicates a node-scoped sequence is satisfied on every participating
	// node and is continuously enforcing ordering for new pods
	PodSequencePhaseActive PodSequencePhase = "Active"
	// PodSequencePhaseCompleted indicates all pods in a cluster-scoped sequence are ready
	PodSequencePhaseCompleted PodSequencePhase = "Completed"
	// PodSequencePhaseFailed indicates the sequence (or, in NodeStatus, a node) failed
	PodSequencePhaseFailed PodSequencePhase = "Failed"
)

// Condition types
const (
	// ConditionReady is True when every participating node satisfies every group
	ConditionReady = "Ready"
	// ConditionDegraded is True when at least one node exceeded gateTimeout
	ConditionDegraded = "Degraded"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=podseq
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="CurrentGroup",type=integer,JSONPath=`.status.currentIndex`
// +kubebuilder:printcolumn:name="Satisfied",type=integer,JSONPath=`.status.summary.satisfiedNodes`,priority=1
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.summary.totalNodes`,priority=1
// +kubebuilder:printcolumn:name="Gated",type=integer,JSONPath=`.status.summary.gatedPods`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PodSequence is the Schema for the podsequences API
type PodSequence struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodSequenceSpec   `json:"spec,omitempty"`
	Status PodSequenceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodSequenceList contains a list of PodSequence
type PodSequenceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodSequence `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodSequence{}, &PodSequenceList{})
}
