package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodGroup represents a group of pods that should start together
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

	// NodeSelector specifies which nodes this group applies to (for node-scoped sequences)
	// When Scope is "Node", pods in this group on each node must be ready before
	// the next group's pods on that same node can start
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// WaitDuration specifies how long to wait after all pods in this group are ready
	// before allowing the next group to start. Useful for initialization, stabilization, or health check time.
	// Format: Go duration (e.g., "30s", "5m", "1h30m")
	// If not specified or "0s", no additional wait is applied
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))*$`
	WaitDuration string `json:"waitDuration,omitempty"`
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

// PodSequenceSpec defines the desired state of PodSequence
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
}

// NodeSequenceStatus tracks the sequencing status for a specific node
type NodeSequenceStatus struct {
	// NodeName is the name of the node
	NodeName string `json:"nodeName"`

	// CurrentIndex is the current group index being processed on this node
	CurrentIndex int `json:"currentIndex"`

	// ReadyPodsInCurrentGroup is the count of ready pods in the current group on this node
	ReadyPodsInCurrentGroup int `json:"readyPodsInCurrentGroup"`

	// Phase indicates the current phase for this node
	Phase PodSequencePhase `json:"phase"`

	// TaintInitializedForGroups tracks which groups' taints have been applied to this node
	// This is a list of group indices that have had their taints initialized on this node
	// Used for recovery: if controller restarts, it knows which taints need to be reapplied
	// +optional
	TaintInitializedForGroups []int `json:"taintInitializedForGroups,omitempty"`
}

// PodSequenceStatus defines the observed state of PodSequence
type PodSequenceStatus struct {
	// CurrentIndex is the index in the sequence that is currently being processed
	// For cluster-scoped sequences, this is the cluster-wide group index
	// For node-scoped sequences, this tracks the minimum index across all nodes
	CurrentIndex int `json:"currentIndex"`

	// Phase indicates the current phase of the sequence
	// Possible values: Pending, InProgress, Completed, Failed
	Phase PodSequencePhase `json:"phase"`

	// NodeStatus tracks per-node sequencing progress (only for node-scoped sequences)
	// +optional
	NodeStatus []NodeSequenceStatus `json:"nodeStatus,omitempty"`

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
	// PodSequencePhaseCompleted indicates all pods in the sequence are ready
	PodSequencePhaseCompleted PodSequencePhase = "Completed"
	// PodSequencePhaseFailed indicates the sequence failed
	PodSequencePhaseFailed PodSequencePhase = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=podseq
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="CurrentGroup",type=integer,JSONPath=`.status.currentIndex`
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
