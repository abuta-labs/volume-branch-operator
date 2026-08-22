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

// Package pool holds the pre-warmed-clone mechanics shared by the BranchPool
// controller (warm-set replenishment) and the Branch controller (the claim
// fast path). A warm clone is a fully provisioned, Bound PVC waiting in the
// holding namespace; claiming one is a metadata operation — no CreateVolume —
// so a Branch is Ready in seconds even on backends that serialize slow clones.
package pool

import (
	"context"
	"fmt"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/branch"
)

const (
	LabelSource    = branch.SourceLabel
	LabelPoolState = "volumes.arbit-tech.com/pool-state"
	LabelClaimant  = "volumes.arbit-tech.com/claimant"
	LabelCloneID   = "volumes.arbit-tech.com/clone-id"

	StateWarm    = "warm"
	StateClaimed = "claimed"
)

// HoldingNamespace is where warm clones live until claimed. Set from the
// --pool-namespace manager flag at startup.
var HoldingNamespace = "branch-pool"

// MaxWarmingDefault caps concurrent warm-clone creations when
// BranchPool.spec.maxWarming is unset. The default suits backends that
// serialize CreateVolume (some serialize to ~1/min — flooding them just
// queues failures); fast-clone backends can raise it per pool. The substrate
// profile takes this seam over in a later phase.
var MaxWarmingDefault int32 = 2

// WarmSet is the trio that materializes one pre-warmed clone PVC.
type WarmSet struct {
	VSC *snapv1.VolumeSnapshotContent // cluster-scoped, Retain, over the source's shared snapshot handle
	VS  *snapv1.VolumeSnapshot        // in the holding namespace, statically bound to the VSC
	PVC *corev1.PersistentVolumeClaim
}

func warmVSCName(cloneID string) string { return "branch-pool-vsc-" + cloneID }
func warmVSName(cloneID string) string  { return "branch-pool-vs-" + cloneID }
func warmPVCName(cloneID string) string { return "branch-warm-" + cloneID }

// BuildWarmSet returns the VSC/VS/PVC for one warm clone keyed by cloneID.
// All three carry an ownerReference to the BranchPool (cluster-scoped), so
// deleting the pool GC-cascades the warm set even if the operator is down;
// the pool finalizer still tears them down in order first, because GC order
// is arbitrary and snapshot-pins-volume backends need volume-before-snapshot.
func BuildWarmSet(bp *volumesv1alpha1.BranchPool, src *volumesv1alpha1.BranchSource, cloneID string) WarmSet {
	labels := map[string]string{
		LabelSource:    src.Name,
		LabelPoolState: StateWarm,
		LabelCloneID:   cloneID,
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: volumesv1alpha1.GroupVersion.String(),
		Kind:       "BranchPool",
		Name:       bp.Name,
		UID:        bp.UID,
		Controller: ptr.To(true),
	}

	vsc := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            warmVSCName(cloneID),
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: snapv1.VolumeSnapshotContentSpec{
			DeletionPolicy: snapv1.VolumeSnapshotContentRetain,
			Driver:         src.Spec.CSIDriver,
			Source: snapv1.VolumeSnapshotContentSource{
				SnapshotHandle: &src.Spec.SnapshotHandle,
			},
			VolumeSnapshotClassName: &src.Spec.VolumeSnapshotClassName,
			VolumeSnapshotRef: corev1.ObjectReference{
				Name:      warmVSName(cloneID),
				Namespace: HoldingNamespace,
			},
		},
	}

	vscName := warmVSCName(cloneID)
	vs := &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            warmVSName(cloneID),
			Namespace:       HoldingNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: snapv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &src.Spec.VolumeSnapshotClassName,
			Source: snapv1.VolumeSnapshotSource{
				VolumeSnapshotContentName: &vscName,
			},
		},
	}

	apiGroup := snapv1.GroupName
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            warmPVCName(cloneID),
			Namespace:       HoldingNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &src.Spec.CloneStorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: branch.CloneSizeRequest},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     warmVSName(cloneID),
			},
		},
	}

	return WarmSet{VSC: vsc, VS: vs, PVC: pvc}
}

// DeleteSnapshotPair deletes a warm clone's seeding VolumeSnapshot and
// VolumeSnapshotContent, ignoring NotFound. Safe by construction: the VSC's
// deletionPolicy is Retain, so the source's shared physical snapshot is
// untouched.
func DeleteSnapshotPair(ctx context.Context, c client.Client, cloneID string) error {
	vs := &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: warmVSName(cloneID), Namespace: HoldingNamespace},
	}
	if err := c.Delete(ctx, vs); client.IgnoreNotFound(err) != nil {
		return err
	}
	vsc := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: warmVSCName(cloneID)},
	}
	if err := c.Delete(ctx, vsc); client.IgnoreNotFound(err) != nil {
		return err
	}
	return nil
}

// buildClaimPVC is the consumer-named PVC statically bound to the claimed PV.
// storageClassName must repeat the PV's class or the binder silently refuses
// the bind; no dataSource — the volume already exists.
func buildClaimPVC(b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource, pvName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Spec.PVCName,
			Namespace: b.Namespace,
			Labels: map[string]string{
				LabelSource:        src.Name,
				branch.BranchLabel: b.Namespace + "." + b.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &src.Spec.CloneStorageClassName,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: branch.CloneSizeRequest},
			},
		},
	}
}

// TryClaim atomically claims a Bound warm clone for b's source and returns
// its PV name and pool PVC. It is NON-DESTRUCTIVE: nothing is deleted or
// rewritten beyond the claim labels, so the caller can durably record the
// claim (Branch.status.claimedVolume) before FinishClaim starts consuming
// the warm clone. found=false is a pool miss (caller falls back to
// on-demand).
//
// The claim is an optimistic label flip guarded by resourceVersion: of two
// concurrent claimants exactly one Update succeeds; the loser sees Conflict
// and tries the next warm clone (or misses).
func TryClaim(ctx context.Context, c client.Client, b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource) (pvName string, poolPVC *corev1.PersistentVolumeClaim, found bool, err error) {
	// Resume: a clone already claimed by this Branch (label flipped, but the
	// claim was never durably recorded before a crash). It must be finished,
	// not abandoned — abandoning it would leak the claimed volume.
	var claimed corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &claimed, client.InNamespace(HoldingNamespace),
		client.MatchingLabels{LabelSource: src.Name, LabelPoolState: StateClaimed, LabelClaimant: string(b.UID)}); err != nil {
		return "", nil, false, err
	}
	for i := range claimed.Items {
		pc := &claimed.Items[i]
		if pc.DeletionTimestamp != nil {
			continue
		}
		return pc.Spec.VolumeName, pc, true, nil
	}

	var warm corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &warm, client.InNamespace(HoldingNamespace),
		client.MatchingLabels{LabelSource: src.Name, LabelPoolState: StateWarm}); err != nil {
		return "", nil, false, err
	}
	for i := range warm.Items {
		pc := &warm.Items[i]
		if pc.Status.Phase != corev1.ClaimBound || pc.DeletionTimestamp != nil || pc.Spec.VolumeName == "" {
			continue
		}
		pc.Labels[LabelPoolState] = StateClaimed
		pc.Labels[LabelClaimant] = string(b.UID)
		if err := c.Update(ctx, pc); err != nil {
			if apierrors.IsConflict(err) {
				continue // another claimant won this clone; try the next
			}
			return "", nil, false, err
		}
		bumpClaimedTotal(ctx, c, src.Name)
		return pc.Spec.VolumeName, pc, true, nil
	}
	return "", nil, false, nil // pool miss
}

// FindClaimedPVC returns the holding-namespace PVC claimed by branchUID, if
// it still exists (FinishClaim deletes it mid-rebind; on resume it may be
// gone, which is fine — the PV name survives in Branch status).
func FindClaimedPVC(ctx context.Context, c client.Client, source, branchUID string) (*corev1.PersistentVolumeClaim, error) {
	var claimed corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &claimed, client.InNamespace(HoldingNamespace),
		client.MatchingLabels{LabelSource: source, LabelPoolState: StateClaimed, LabelClaimant: branchUID}); err != nil {
		return nil, err
	}
	for i := range claimed.Items {
		if claimed.Items[i].DeletionTimestamp == nil {
			return &claimed.Items[i], nil
		}
	}
	return nil, nil
}

// FinishClaim rebinds the claimed PV into the Branch's namespace. Idempotent —
// safe to re-enter at any step; the caller has durably recorded pvName in
// Branch status BEFORE this runs, because step 2 destroys the warm PVC — the
// only other marker of the claim. The order is load-bearing:
//
//  1. PV reclaim policy -> Retain, so deleting the holding-namespace PVC
//     releases the PV instead of destroying the volume. It STAYS Retain for
//     the clone's whole life; teardown flips it to Delete at the end so the
//     CSI driver destroys the volume exactly once, on purpose.
//  2. Delete the warm PVC (PV -> Released) and its spent seeding VS/VSC.
//  3. Point the PV's ClaimRef at the consumer-named PVC BEFORE that PVC
//     exists. Never clear ClaimRef to nil: a Released/Available PV with no
//     ClaimRef is up for grabs — the PV binder can hand it to ANY matching
//     pending PVC in the cluster, and that victim then wedges Pending forever
//     ("volume already bound to a different claim") when its own volume
//     arrives. A ClaimRef naming the future PVC (no UID — the binder fills
//     it) keeps the PV reserved for exactly its claimant through the whole
//     rebind window.
//  4. Create the consumer PVC; the binder completes the pre-bind.
func FinishClaim(ctx context.Context, c client.Client, b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource, pvName string, poolPVC *corev1.PersistentVolumeClaim) error {
	if pvName == "" {
		return fmt.Errorf("pool: FinishClaim called with an empty PV name")
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		return err
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		if err := c.Update(ctx, pv); err != nil {
			return err
		}
	}
	if poolPVC != nil {
		if err := c.Delete(ctx, poolPVC); client.IgnoreNotFound(err) != nil {
			return err
		}
		if cloneID := poolPVC.Labels[LabelCloneID]; cloneID != "" {
			if err := DeleteSnapshotPair(ctx, c, cloneID); err != nil {
				return err
			}
		}
	}
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != b.Spec.PVCName || pv.Spec.ClaimRef.Namespace != b.Namespace {
		pv.Spec.ClaimRef = &corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  b.Namespace,
			Name:       b.Spec.PVCName,
		}
		if err := c.Update(ctx, pv); err != nil {
			return err
		}
	}
	if err := c.Create(ctx, buildClaimPVC(b, src, pvName)); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

// bumpClaimedTotal increments claimedTotal on the pools of this source.
// Best-effort telemetry: a conflict or racing update may drop an increment;
// correctness never depends on the counter.
func bumpClaimedTotal(ctx context.Context, c client.Client, source string) {
	var pools volumesv1alpha1.BranchPoolList
	if err := c.List(ctx, &pools); err != nil {
		return
	}
	for i := range pools.Items {
		bp := &pools.Items[i]
		if bp.Spec.Source != source {
			continue
		}
		bp.Status.ClaimedTotal++
		_ = c.Status().Update(ctx, bp)
		return
	}
}

// ReclaimClaimedFor sweeps the holding namespace for clones claimed by
// branchUID that never finished rebinding (label flipped, consumer PVC never
// created) and reclaims them: pool PVC, its PV (flipped to Delete so the CSI
// driver destroys the volume), and the spent snapshot pair. Used by Branch
// teardown so a crash between label flip and PVC creation cannot leak a
// volume.
func ReclaimClaimedFor(ctx context.Context, c client.Client, source string, branchUID string) error {
	var claimed corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &claimed, client.InNamespace(HoldingNamespace),
		client.MatchingLabels{LabelSource: source, LabelPoolState: StateClaimed, LabelClaimant: branchUID}); err != nil {
		return err
	}
	for i := range claimed.Items {
		if err := ReclaimCloneSet(ctx, c, &claimed.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// ReclaimCloneSet deletes a warm/claimed clone's PVC, its PV (reclaim policy
// forced to Delete first, so the CSI driver destroys the backing volume), and
// its seeding snapshot pair.
func ReclaimCloneSet(ctx context.Context, c client.Client, pc *corev1.PersistentVolumeClaim) error {
	cloneID := pc.Labels[LabelCloneID]
	pvName := pc.Spec.VolumeName
	if pc.DeletionTimestamp == nil {
		if err := c.Delete(ctx, pc); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	if pvName != "" {
		if err := DeletePV(ctx, c, pvName); err != nil {
			return err
		}
	}
	if cloneID != "" {
		return DeleteSnapshotPair(ctx, c, cloneID)
	}
	return nil
}

// DeletePV forces a PV's reclaim policy to Delete and deletes it, so the CSI
// driver destroys the backing volume. NotFound is fine — already gone.
func DeletePV(ctx context.Context, c client.Client, pvName string) error {
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		if err := c.Update(ctx, pv); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return client.IgnoreNotFound(c.Delete(ctx, pv))
}
