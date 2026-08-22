/*
Copyright 2026.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BranchSpec defines one claimed clone of a BranchSource, terminating at a
// Bound PVC in the Branch's own namespace. The consumer names the PVC
// (preserves ClaimRef pre-bind; the engine never picks names).
type BranchSpec struct {
	// source is the name of the BranchSource to branch from.
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`

	// pvcName is the name of the PVC the engine must produce in this
	// Branch's namespace.
	// +kubebuilder:validation:MinLength=1
	PVCName string `json:"pvcName"`

	// resetToken, when changed, discards the current clone and re-branches
	// from the same source. Opaque to the engine.
	// +optional
	ResetToken string `json:"resetToken,omitempty"`

	// ttl, when set, deletes the Branch (and its PVC) this long after it
	// becomes Ready. Consumers with their own reaping policy omit it.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// BranchPhase is the lifecycle state of a Branch.
// +kubebuilder:validation:Enum=Pending;Cloning;Ready;Failed
type BranchPhase string

const (
	BranchPending BranchPhase = "Pending"
	BranchCloning BranchPhase = "Cloning"
	BranchReady   BranchPhase = "Ready"
	BranchFailed  BranchPhase = "Failed"
)

// BranchProvisioning records how the clone was produced; it drives teardown
// ordering on delete.
// +kubebuilder:validation:Enum=pool;ondemand
type BranchProvisioning string

const (
	ProvisionedFromPool BranchProvisioning = "pool"
	ProvisionedOnDemand BranchProvisioning = "ondemand"
)

// BranchStatus defines the observed state of Branch.
type BranchStatus struct {
	// phase ends at Ready when the named PVC is Bound. What runs on the
	// volume afterwards is the consumer's business.
	// +optional
	Phase BranchPhase `json:"phase,omitempty"`

	// provisioning records the claim path taken (pool fast path vs
	// on-demand clone).
	// +optional
	Provisioning BranchProvisioning `json:"provisioning,omitempty"`

	// clonedBytes is the clone's logical size, when known.
	// +optional
	ClonedBytes int64 `json:"clonedBytes,omitempty"`

	// observedResetToken is the last resetToken acted upon.
	// +optional
	ObservedResetToken string `json:"observedResetToken,omitempty"`

	// expiresAt is when spec.ttl will reap this Branch (set once Ready).
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// message carries a human-readable reason for Failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=`.spec.pvcName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Via",type=string,JSONPath=`.status.provisioning`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Branch is one claimed copy-on-write clone of a BranchSource.
type Branch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BranchSpec   `json:"spec,omitempty"`
	Status BranchStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BranchList contains a list of Branch.
type BranchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Branch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Branch{}, &BranchList{})
}
