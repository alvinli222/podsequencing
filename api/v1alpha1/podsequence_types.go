package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodGroup represents a group of pods that should start together
type PodGroup struct {
	// Name is an optional name for the pod group (for identification purposes)
	// +optional
	Name string `json:"name,omitempty"`

	// Pods is the list of pod names in this group
	// All pods in the group will have their scheduling gates removed together
	// +kubebuilder:validation:MinItems=1
	Pods []string `json:"pods"`
}

// PodSequenceSpec defines the desired state of PodSequence
type PodSequenceSpec struct {
	// Sequence is an ordered list of pod names that should be scheduled sequentially
	// Each pod in the list will only have its scheduling gate removed after the previous
	// pod becomes ready
	// NOTE: This field is deprecated in favor of PodGroups for more flexibility
	// +optional
	Sequence []string `json:"sequence,omitempty"`

	// PodGroups is an ordered list of pod groups that should be scheduled sequentially
	// Each group contains one or more pods. All pods in a group will have their scheduling
	// gates removed together, and the next group will only start after all pods in the
	// current group are ready
	// +kubebuilder:validation:MinItems=1
	// +optional
	PodGroups []PodGroup `json:"podGroups,omitempty"`

	// Namespace is the namespace where the pods are located
	// If not specified, defaults to the namespace of the PodSequence resource
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// SchedulingGateName is the name of the scheduling gate to manage
	// Defaults to "podsequence.example.com/sequence-gate"
	// +optional
	SchedulingGateName string `json:"schedulingGateName,omitempty"`
}

// PodSequenceStatus defines the observed state of PodSequence
type PodSequenceStatus struct {
	// CurrentIndex is the index in the sequence that is currently being processed
	// For PodGroups, this refers to the current group index
	CurrentIndex int `json:"currentIndex"`

	// Phase indicates the current phase of the sequence
	// Possible values: Pending, InProgress, Completed, Failed
	Phase PodSequencePhase `json:"phase"`

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
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.currentIndex`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.spec.sequence[*]`
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
