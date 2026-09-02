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

// Package branch builds the per-branch storage objects: a pre-provisioned
// VolumeSnapshotContent over the source's shared snapshot handle, a
// namespaced VolumeSnapshot bound to it, and the clone PVC. A
// VolumeSnapshotContent binds 1:1 to a VolumeSnapshot, so every branch mints
// its own pair even though they all reference the same physical snapshot.
package branch

import (
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	volumesv1alpha1 "github.com/abuta-labs/volume-branch-operator/api/v1alpha1"
)

// Labels stamped on engine-created objects, so a BranchSource delete can
// sweep any cluster-scoped leftovers that outlived their Branch.
const (
	SourceLabel = "volumes.abuta-labs.com/source"
	BranchLabel = "volumes.abuta-labs.com/branch"
)

// VSCName is the per-branch VolumeSnapshotContent name. UID-keyed: the
// object is cluster-scoped, so the name must be globally unique even across
// same-named Branches in different namespaces.
func VSCName(b *volumesv1alpha1.Branch) string { return "branch-vsc-" + string(b.UID) }

// VSName is the per-branch VolumeSnapshot name (namespaced beside the Branch).
func VSName(b *volumesv1alpha1.Branch) string { return "branch-vs-" + b.Name }

func labels(b *volumesv1alpha1.Branch) map[string]string {
	return map[string]string{
		SourceLabel: b.Spec.Source,
		BranchLabel: b.Namespace + "." + b.Name,
	}
}

// BuildVSC returns the cluster-scoped, pre-provisioned VolumeSnapshotContent
// referencing the source's shared snapshot handle, statically bound to this
// branch's VolumeSnapshot.
//
// deletionPolicy is Retain: deleting a Delete-policy VSC makes the
// external-snapshotter delete the PHYSICAL snapshot, which every other branch
// of the same source still depends on. Only source-level reclaim may ever do
// that; per-branch objects must always detach without touching the backend.
func BuildVSC(b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource) *snapv1.VolumeSnapshotContent {
	return &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:   VSCName(b),
			Labels: labels(b),
		},
		Spec: snapv1.VolumeSnapshotContentSpec{
			DeletionPolicy: snapv1.VolumeSnapshotContentRetain,
			Driver:         src.Spec.CSIDriver,
			Source: snapv1.VolumeSnapshotContentSource{
				SnapshotHandle: &src.Spec.SnapshotHandle,
			},
			VolumeSnapshotClassName: &src.Spec.VolumeSnapshotClassName,
			VolumeSnapshotRef: corev1.ObjectReference{
				Name:      VSName(b),
				Namespace: b.Namespace,
			},
		},
	}
}

// BuildVS returns the namespaced VolumeSnapshot statically bound to the
// per-branch VolumeSnapshotContent.
func BuildVS(b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource) *snapv1.VolumeSnapshot {
	vscName := VSCName(b)
	return &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      VSName(b),
			Namespace: b.Namespace,
			Labels:    labels(b),
		},
		Spec: snapv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &src.Spec.VolumeSnapshotClassName,
			Source: snapv1.VolumeSnapshotSource{
				VolumeSnapshotContentName: &vscName,
			},
		},
	}
}

// BuildPVC returns the clone PVC: the consumer-named claim, in the Branch's
// namespace, provisioned from the per-branch VolumeSnapshot. size is the
// capacity request, computed by the source's substrate profile (sentinel
// placeholder vs the source's actual size).
func BuildPVC(b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource, size resource.Quantity) *corev1.PersistentVolumeClaim {
	apiGroup := snapv1.GroupName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Spec.PVCName,
			Namespace: b.Namespace,
			Labels:    labels(b),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &src.Spec.CloneStorageClassName,
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     VSName(b),
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: size,
				},
			},
		},
	}
}
