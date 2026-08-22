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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloneSizeMode selects how clone PVC capacity requests are computed.
// +kubebuilder:validation:Enum=sentinel;actual
type CloneSizeMode string

const (
	// CloneSizeModeSentinel requests a small fixed placeholder. For drivers
	// whose snapshot-sourced clones are always full-size views of their
	// parent, the request is never honored, and asking for the real size
	// would only trip quota checks.
	CloneSizeModeSentinel CloneSizeMode = "sentinel"
	// CloneSizeModeActual requests the source's real size (rounded up).
	// Required on drivers that enforce request >= the snapshot's restore
	// size.
	CloneSizeModeActual CloneSizeMode = "actual"
)

// ProfileOverrides is a per-source override of the built-in substrate
// profile for its CSI driver. Every field is optional; unset fields keep the
// built-in value. The effective result is published in
// status.resolvedProfile.
type ProfileOverrides struct {
	// snapshotPinsVolume declares that on this backend a snapshot pins its
	// parent volume, so teardown must remove volume objects before snapshot
	// objects.
	// +optional
	SnapshotPinsVolume *bool `json:"snapshotPinsVolume,omitempty"`

	// cloneSizeMode selects sentinel or actual clone sizing.
	// +optional
	CloneSizeMode *CloneSizeMode `json:"cloneSizeMode,omitempty"`

	// cloneSizeSentinel is the placeholder capacity requested in sentinel
	// mode (and the fallback in actual mode while the source size is still
	// unknown).
	// +optional
	CloneSizeSentinel *resource.Quantity `json:"cloneSizeSentinel,omitempty"`

	// maxWarmingDefault caps concurrent warm-clone creations for pools of
	// this source when BranchPool.spec.maxWarming is unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxWarmingDefault *int32 `json:"maxWarmingDefault,omitempty"`
}

// ResolvedProfile is the effective substrate profile for a source:
// the built-in profile for its CSI driver with spec.profile overrides
// applied.
type ResolvedProfile struct {
	SnapshotPinsVolume bool          `json:"snapshotPinsVolume"`
	CloneSizeMode      CloneSizeMode `json:"cloneSizeMode"`
	CloneSizeSentinel  string        `json:"cloneSizeSentinel"`
	MaxWarmingDefault  int32         `json:"maxWarmingDefault"`
}

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

	// profile overrides the built-in substrate profile for spec.csiDriver.
	// +optional
	Profile *ProfileOverrides `json:"profile,omitempty"`

	// sizeBytes declares the snapshot's logical size. Optional: normally the
	// engine discovers it from a VolumeSnapshotContent whose snapshotHandle
	// matches (the snapshot's originating content carries restoreSize).
	// Declare it when that content no longer exists — actual-mode sizing
	// cannot proceed with an unknown size.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// BranchSourcePhase is the validation state of a BranchSource.
// +kubebuilder:validation:Enum=Pending;Ready;Invalid
type BranchSourcePhase string

const (
	BranchSourcePending BranchSourcePhase = "Pending"
	BranchSourceReady   BranchSourcePhase = "Ready"
	BranchSourceInvalid BranchSourcePhase = "Invalid"
)

// Reasons for the BranchSource Ready condition.
const (
	// ReasonValidated: classes exist, drivers agree, and (in actual sizing
	// mode) the size is known — clones can be created from this source now.
	ReasonValidated = "Validated"
	// ReasonClassMissing: the referenced StorageClass or VolumeSnapshotClass
	// does not exist.
	ReasonClassMissing = "ClassMissing"
	// ReasonDriverMismatch: a referenced class belongs to a different CSI
	// driver than spec.csiDriver.
	ReasonDriverMismatch = "DriverMismatch"
	// ReasonValidationError: validation could not complete (transient API
	// error); it will be retried.
	ReasonValidationError = "ValidationError"
	// ReasonSizeUnknown: the source validates but actual-mode sizing is
	// holding clone creation until the snapshot's size is known (discovered
	// from its originating VolumeSnapshotContent, or declared in
	// spec.sizeBytes).
	ReasonSizeUnknown = "SizeUnknown"
)

// BranchSourceStatus defines the observed state of BranchSource.
type BranchSourceStatus struct {
	// phase is the validation state of the source.
	// +optional
	Phase BranchSourcePhase `json:"phase,omitempty"`

	// conditions describe the source's readiness for branching. The Ready
	// condition is stricter than phase: it is True only when clones can be
	// created right now (so a validated source holding for size discovery is
	// phase=Ready but Ready=False/SizeUnknown).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// sizeBytes is the logical size of the source snapshot, when known.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// message carries a human-readable reason for Invalid.
	// +optional
	Message string `json:"message,omitempty"`

	// resolvedProfile is the effective substrate profile: the built-in
	// profile for spec.csiDriver with spec.profile overrides applied.
	// +optional
	ResolvedProfile *ResolvedProfile `json:"resolvedProfile,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Sizing",type=string,JSONPath=`.status.resolvedProfile.cloneSizeMode`
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
