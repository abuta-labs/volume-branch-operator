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

// BranchSourceSpec defines an immutable snapshot handle to branch from.
type BranchSourceSpec struct {
	// snapshotHandle is the CSI snapshot handle (driver-specific id) of the
	// immutable source snapshot. Set once; the source is immutable.
	// +kubebuilder:validation:MinLength=1
	SnapshotHandle string `json:"snapshotHandle"`

	// csiDriver is the CSI driver name the snapshot handle belongs to
	// (e.g. fsx.openzfs.csi.aws.com, zfs.csi.openebs.io).
	// +kubebuilder:validation:MinLength=1
	CSIDriver string `json:"csiDriver"`

	// cloneStorageClassName is the StorageClass used for PVCs cloned from
	// this source.
	// +kubebuilder:validation:MinLength=1
	CloneStorageClassName string `json:"cloneStorageClassName"`

	// volumeSnapshotClassName is the VolumeSnapshotClass used when
	// materializing per-branch VolumeSnapshotContent/VolumeSnapshot pairs.
	// +kubebuilder:validation:MinLength=1
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName"`
}

// BranchSourcePhase is the validation state of a BranchSource.
// +kubebuilder:validation:Enum=Pending;Ready;Invalid
type BranchSourcePhase string

const (
	BranchSourcePending BranchSourcePhase = "Pending"
	BranchSourceReady   BranchSourcePhase = "Ready"
	BranchSourceInvalid BranchSourcePhase = "Invalid"
)

// BranchSourceStatus defines the observed state of BranchSource.
type BranchSourceStatus struct {
	// phase is the validation state of the source.
	// +optional
	Phase BranchSourcePhase `json:"phase,omitempty"`

	// sizeBytes is the logical size of the source snapshot, when known.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// message carries a human-readable reason for Invalid.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BranchSource is a named, immutable snapshot to branch volumes from.
type BranchSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BranchSourceSpec   `json:"spec,omitempty"`
	Status BranchSourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BranchSourceList contains a list of BranchSource.
type BranchSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BranchSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BranchSource{}, &BranchSourceList{})
}
