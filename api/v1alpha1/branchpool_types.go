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

// BranchPoolSpec defines a warm set of pre-cloned volumes against a source.
type BranchPoolSpec struct {
	// source is the name of the BranchSource this pool pre-clones.
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`

	// targetWarm is the number of ready-to-claim clones to keep warm.
	// Backends with fast clones may want 0 (claim on demand is cheap there).
	// +kubebuilder:validation:Minimum=0
	TargetWarm int32 `json:"targetWarm"`

	// maxWarming caps concurrent clone creations. Unset means the substrate
	// profile default (backends like FSx serialize CreateVolume and need a
	// low cap; fast-clone backends can go high).
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxWarming *int32 `json:"maxWarming,omitempty"`
}

// BranchPoolStatus defines the observed state of BranchPool.
type BranchPoolStatus struct {
	// warm is the number of ready, unclaimed clones.
	// +optional
	Warm int32 `json:"warm,omitempty"`

	// warming is the number of clones currently being created.
	// +optional
	Warming int32 `json:"warming,omitempty"`

	// claimedTotal counts claims served from this pool over its lifetime.
	// +optional
	ClaimedTotal int64 `json:"claimedTotal,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Warm",type=integer,JSONPath=`.status.warm`
// +kubebuilder:printcolumn:name="Target",type=integer,JSONPath=`.spec.targetWarm`
// +kubebuilder:printcolumn:name="Warming",type=integer,JSONPath=`.status.warming`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BranchPool keeps N pre-warmed clones of a BranchSource so a Branch claim
// is a label flip instead of a CreateVolume call.
type BranchPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BranchPoolSpec   `json:"spec,omitempty"`
	Status BranchPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BranchPoolList contains a list of BranchPool.
type BranchPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BranchPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BranchPool{}, &BranchPoolList{})
}
