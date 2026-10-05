/*
Copyright 2026 Example.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// BootstrapLabelKey is applied by the infra layer (or tests) to join a node to a pool.
	BootstrapLabelKey = "nodes.example.com/nodepool"

	// ClaimedByAnnotation records which NodePool currently owns a node.
	ClaimedByAnnotation = "nodes.example.com/claimed-by"

	// ManagedLabelsAnnotation is a JSON array of label keys this controller applied.
	ManagedLabelsAnnotation = "nodes.example.com/managed-labels"

	// ManagedTaintsAnnotation is a JSON array of "key:effect" taints this controller applied.
	ManagedTaintsAnnotation = "nodes.example.com/managed-taints"

	// CordonedAnnotation is set when this controller cordoned the node so we can uncordon on recovery.
	CordonedAnnotation = "nodes.example.com/cordoned"

	// FinalizerName blocks NodePool deletion until managed labels/taints are stripped from Ready members.
	FinalizerName = "nodes.example.com/nodepool"

	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionFailed      = "Failed"
)

// UnreadyAction is taken after a member has been NotReady longer than gracePeriod.
// +kubebuilder:validation:Enum=Remove;Cordon;Ignore
type UnreadyAction string

const (
	UnreadyActionRemove UnreadyAction = "Remove"
	UnreadyActionCordon UnreadyAction = "Cordon"
	UnreadyActionIgnore UnreadyAction = "Ignore"
)

// NodePhase is the controller's view of a member node.
// +kubebuilder:validation:Enum=Ready;NotReady;Draining;Removing
type NodePhase string

const (
	NodePhaseReady    NodePhase = "Ready"
	NodePhaseNotReady NodePhase = "NotReady"
	NodePhaseDraining NodePhase = "Draining"
	NodePhaseRemoving NodePhase = "Removing"
)

// Taint is a subset of corev1.Taint used in NodePool spec (no TimeAdded).
type Taint struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// +optional
	Value string `json:"value,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
	Effect corev1.TaintEffect `json:"effect"`
}

func (t Taint) ToCore() corev1.Taint {
	return corev1.Taint{Key: t.Key, Value: t.Value, Effect: t.Effect}
}

// UnreadyPolicy controls reactive scale-down when a member stops heartbeating.
type UnreadyPolicy struct {
	// GracePeriod is how long a node may stay NotReady (Ready=False or Unknown) before action.
	// +kubebuilder:validation:Required
	GracePeriod metav1.Duration `json:"gracePeriod"`

	// Action taken after gracePeriod. Required so we never silently delete nodes.
	// +kubebuilder:validation:Required
	Action UnreadyAction `json:"action"`

	// MaxConcurrentRemovals caps how many members may be in Draining or Removing at once.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxConcurrentRemovals int32 `json:"maxConcurrentRemovals,omitempty"`

	// DrainTimeout is how long to wait for eviction (respecting PDBs) before giving up.
	// Defaults to 5m in the controller if omitted.
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`

	// ForceDeletePodsAfterDrainTimeout, if true, grace-period-0 deletes remaining pods
	// after DrainTimeout — but only when the Node Ready condition is Unknown (kubelet
	// is likely gone). Default false: we delete the Node object and let pod GC finish.
	// +optional
	ForceDeletePodsAfterDrainTimeout bool `json:"forceDeletePodsAfterDrainTimeout,omitempty"`
}

func (p UnreadyPolicy) DrainTimeoutDuration() metav1.Duration {
	if p.DrainTimeout != nil {
		return *p.DrainTimeout
	}
	return metav1.Duration{Duration: 5 * time.Minute}
}

// NodePoolSpec defines the desired state of NodePool.
type NodePoolSpec struct {
	// Image is informational for the infra layer; this controller does not provision VMs.
	// +optional
	Image string `json:"image,omitempty"`

	// Labels applied to member nodes. Keys reserved by Kubernetes or used as the
	// bootstrap label are ignored.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Taints applied to member nodes. System taints (node.kubernetes.io/*) are ignored.
	// +optional
	Taints []Taint `json:"taints,omitempty"`

	// UnreadyPolicy is required: callers must choose Remove, Cordon, or Ignore.
	// +kubebuilder:validation:Required
	UnreadyPolicy UnreadyPolicy `json:"unreadyPolicy"`
}

// NodeMemberStatus is the observed state of one member.
type NodeMemberStatus struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Phase NodePhase `json:"phase"`

	// NotReadySince is when this controller first observed NotReady for the current incident.
	// Persisted so grace period survives process restart. Nil when Ready.
	// +optional
	NotReadySince *metav1.Time `json:"notReadySince,omitempty"`

	// DrainStartedAt is when Draining began. Persisted so drain timeout survives restart.
	// +optional
	DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`
}

// NodePoolStatus defines the observed state of NodePool.
type NodePoolStatus struct {
	// ReadyNodes is the count of members whose Node Ready condition is True.
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// Nodes is the per-member observed state.
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []NodeMemberStatus `json:"nodes,omitempty"`

	// ObservedGeneration is the spec generation last successfully reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow metav1.Condition (Ready, Progressing, Failed).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=npool
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="ReadyNodes",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NodePool groups nodes that share OS image, labels, and taints.
type NodePool struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec NodePoolSpec `json:"spec"`

	// +optional
	Status NodePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NodePoolList contains a list of NodePool.
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NodePool{}, &NodePoolList{})
		return nil
	})
}
