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

package controller

import (
	"context"
	"time"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	volumesv1alpha1 "github.com/abuta-labs/volume-branch-operator/api/v1alpha1"
	"github.com/abuta-labs/volume-branch-operator/internal/branch"
	"github.com/abuta-labs/volume-branch-operator/internal/pool"
	"github.com/abuta-labs/volume-branch-operator/internal/profile"
)

// BranchFinalizer gates Branch deletion on clone teardown: PVC, then
// VolumeSnapshot, then VolumeSnapshotContent, strictly in that order.
const BranchFinalizer = "volumes.abuta-labs.com/branch-teardown"

// BranchReconciler reconciles a Branch object.
type BranchReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branchsources,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots;volumesnapshotcontents,verbs=get;list;watch;create;delete

// Reconcile drives a Branch to a Bound PVC — claimed from a warm pool when
// one has stock, or provisioned on demand (VSC + VS + PVC from the source's
// snapshot handle) otherwise.
func (r *BranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b volumesv1alpha1.Branch
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !b.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &b)
	}
	if controllerutil.AddFinalizer(&b, BranchFinalizer) {
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}

	// TTL reap: once expired, delete the Branch; the finalizer tears down the
	// clone objects like any other delete.
	if b.Status.ExpiresAt != nil && !time.Now().Before(b.Status.ExpiresAt.Time) {
		if err := r.Delete(ctx, &b); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	// Resolve the source; it must exist and be Ready before any clone object
	// is created.
	var src volumesv1alpha1.BranchSource
	if err := r.Get(ctx, client.ObjectKey{Name: b.Spec.Source}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setPhase(ctx, &b, volumesv1alpha1.ReasonWaitingForSource, "BranchSource not found")
		}
		return ctrl.Result{}, err
	}
	if src.Status.Phase != volumesv1alpha1.BranchSourceReady {
		return r.setPhase(ctx, &b, volumesv1alpha1.ReasonWaitingForSource, "waiting for BranchSource to become Ready")
	}

	// Reset: a changed token discards the current clone and re-runs the flow.
	// Baseline vs reset is decided by whether a clone exists to tear down
	// (status.provisioning set), never by token values: an empty token is a
	// legitimate baseline, a branch born without a token must still reset
	// when its first real token arrives ("" -> "t1"), and so must "t1" -> "".
	if b.Status.Provisioning == "" {
		b.Status.ObservedResetToken = b.Spec.ResetToken
	} else if b.Status.ObservedResetToken != b.Spec.ResetToken {
		if err := r.teardownClone(ctx, &b, profile.Resolve(&src).SnapshotPinsVolume); err != nil {
			return ctrl.Result{}, err
		}
		b.Status.ObservedResetToken = b.Spec.ResetToken
		b.Status.Phase = volumesv1alpha1.BranchCloning
		b.Status.Provisioning = ""
		b.Status.ClaimedVolume = ""
		b.Status.ClonedBytes = 0
		b.Status.ExpiresAt = nil
		b.Status.Message = ""
		if err := r.Status().Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
		// Give the deletes a beat to complete before re-creating same-named
		// objects; the requeue re-enters the create path below.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Actual-mode sizing may not guess: a sentinel-sized request below the
	// snapshot's restore size is refused by size-enforcing drivers and the
	// mis-sized PVC wedges permanently. Hold until the source publishes its
	// size (the source controller's probe is discovering it).
	if prof := profile.Resolve(&src); prof.NeedsSize() && src.Status.SizeBytes == 0 && b.Status.Provisioning == "" {
		return r.setPhase(ctx, &b, volumesv1alpha1.ReasonWaitingForSize, "waiting for the source size (actual-mode sizing)")
	}

	if err := r.ensureClone(ctx, &b, &src); err != nil {
		return ctrl.Result{}, err
	}

	// Ready means exactly: the consumer-named PVC is Bound. What runs on the
	// volume afterwards is the consumer's business.
	var pvc corev1.PersistentVolumeClaim
	bound := false
	if err := r.Get(ctx, client.ObjectKey{Name: b.Spec.PVCName, Namespace: b.Namespace}, &pvc); err == nil {
		bound = pvc.Status.Phase == corev1.ClaimBound
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if bound {
		b.Status.Phase = volumesv1alpha1.BranchReady
		b.Status.ClonedBytes = src.Status.SizeBytes
		b.Status.Message = ""
		// The TTL clock starts at Ready, latched once. (TTL extension policy —
		// moving an existing deadline — is deliberately not implemented yet.)
		if b.Spec.TTL != nil && b.Status.ExpiresAt == nil {
			t := metav1.NewTime(time.Now().Add(b.Spec.TTL.Duration))
			b.Status.ExpiresAt = &t
		}
		setCondition(&b.Status.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             volumesv1alpha1.ReasonProvisioned,
			Message:            "PVC " + b.Spec.PVCName + " is Bound (" + string(b.Status.Provisioning) + ")",
			ObservedGeneration: b.Generation,
		})
	} else {
		b.Status.Phase = volumesv1alpha1.BranchCloning
		setCondition(&b.Status.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             volumesv1alpha1.ReasonCloning,
			Message:            "waiting for PVC " + b.Spec.PVCName + " to bind",
			ObservedGeneration: b.Generation,
		})
	}
	if err := r.Status().Update(ctx, &b); err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case !bound:
		// envtest and slow backends: poll for the bind as well as watching.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	case b.Status.ExpiresAt != nil:
		return ctrl.Result{RequeueAfter: time.Until(b.Status.ExpiresAt.Time)}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// ensureClone provisions the clone. The pool fast path is tried first: a
// pre-warmed Bound PVC is claimed by metadata alone — no CreateVolume — so
// the Branch is Ready in seconds even on backends with slow clones. On a
// pool miss the on-demand object set is created instead. status.provisioning
// records the path taken because the two leave different objects behind and
// teardown depends on it.
func (r *BranchReconciler) ensureClone(ctx context.Context, b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource) error {
	// A terminating consumer PVC is a previous clone still being deleted
	// (reset in flight). Provisioning anything now would either re-adopt the
	// corpse or collide with its name — wait for the deletion to finish; the
	// caller's requeue re-enters here.
	var existing corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: b.Spec.PVCName, Namespace: b.Namespace}, &existing); err == nil {
		if existing.DeletionTimestamp != nil {
			return nil
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	// Established or recorded pool claim: resume the rebind. The PV name in
	// status is authoritative — the warm PVC may already be destroyed.
	if b.Status.ClaimedVolume != "" {
		poolPVC, err := pool.FindClaimedPVC(ctx, r.Client, src.Name, string(b.UID))
		if err != nil {
			return err
		}
		return pool.FinishClaim(ctx, r.Client, b, src, b.Status.ClaimedVolume, poolPVC)
	}
	if b.Status.Provisioning == "" {
		// The claim is two-phase: TryClaim only flips labels (non-destructive),
		// then the claim is durably recorded on the Branch, and only then does
		// FinishClaim start consuming the warm clone. A failure between flip
		// and record is recovered by TryClaim's resume path (claimant label);
		// a failure after the record is recovered by the status branch above.
		// Recording AFTER destruction would lose the claim on a mid-rebind
		// conflict and silently fall back to on-demand, leaking the PV.
		pvName, poolPVC, found, err := pool.TryClaim(ctx, r.Client, b, src)
		if err != nil {
			return err
		}
		if found {
			b.Status.ClaimedVolume = pvName
			b.Status.Provisioning = volumesv1alpha1.ProvisionedFromPool
			b.Status.Phase = volumesv1alpha1.BranchCloning
			if err := r.Status().Update(ctx, b); err != nil {
				return err
			}
			return pool.FinishClaim(ctx, r.Client, b, src, pvName, poolPVC)
		}
		b.Status.Provisioning = volumesv1alpha1.ProvisionedOnDemand
		b.Status.Phase = volumesv1alpha1.BranchCloning
		if err := r.Status().Update(ctx, b); err != nil {
			return err
		}
	}

	// The VSC is cluster-scoped: no owner reference possible; the labels let
	// source-level teardown sweep it if this Branch's finalizer never ran.
	if err := r.createIfAbsent(ctx, branch.BuildVSC(b, src)); err != nil {
		return err
	}
	vs := branch.BuildVS(b, src)
	if err := controllerutil.SetControllerReference(b, vs, r.Scheme); err != nil {
		return err
	}
	if err := r.createIfAbsent(ctx, vs); err != nil {
		return err
	}
	size := profile.Resolve(src).SizeRequest(src.Status.SizeBytes)
	pvc := branch.BuildPVC(b, src, size)
	if err := controllerutil.SetControllerReference(b, pvc, r.Scheme); err != nil {
		return err
	}
	return r.createIfAbsent(ctx, pvc)
}

// teardownClone deletes whatever the provisioning path left behind.
//
// Pool provenance: the consumer PVC and its retained PV (flipped to Delete so
// the CSI driver destroys the volume — it was deliberately kept Retain across
// the claim rebind). The seeding snapshot pair was already deleted at claim
// time, so there is nothing snapshot-shaped to remove.
//
// On-demand provenance: PVC first (the volume), then VolumeSnapshot, then
// VolumeSnapshotContent. On backends where a snapshot pins its parent volume,
// deleting snapshot objects before the volume can wedge the backend-side
// cleanup — the volume must go first. Deletes are fired here; completion is
// confirmed by reconcileDelete.
//
// Empty provenance still sweeps the holding namespace for a clone this
// Branch claimed (label flipped) before ever persisting provisioning=pool —
// a crash in that window must not leak the claimed volume.
// teardownClone deletes the Branch's clone objects. pinsVolume is the
// source profile's SnapshotPinsVolume: with it set, volume objects are
// deleted before snapshot objects (per-branch snapshot objects here are all
// Retain-policy — Kubernetes-object-only — so today the order is a
// consistency discipline; it becomes load-bearing wherever a teardown
// performs physical deletions).
func (r *BranchReconciler) teardownClone(ctx context.Context, b *volumesv1alpha1.Branch, pinsVolume bool) error {
	if b.Status.Provisioning != volumesv1alpha1.ProvisionedOnDemand {
		if err := pool.ReclaimClaimedFor(ctx, r.Client, b.Spec.Source, string(b.UID)); err != nil {
			return err
		}
	}
	if b.Status.Provisioning == volumesv1alpha1.ProvisionedFromPool {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Name: b.Spec.PVCName, Namespace: b.Namespace}, pvc); err == nil {
			pvName := pvc.Spec.VolumeName
			if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if pvName != "" {
				if err := pool.DeletePV(ctx, r.Client, pvName); err != nil {
					return err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		// The recorded claim is authoritative: it covers the window where the
		// consumer PVC never got created (or is already gone) but the PV was
		// claimed. Normally the same PV as above — DeletePV is idempotent.
		if b.Status.ClaimedVolume != "" {
			if err := pool.DeletePV(ctx, r.Client, b.Status.ClaimedVolume); err != nil {
				return err
			}
		}
		return nil
	}

	deletePVC := func() error {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: b.Spec.PVCName, Namespace: b.Namespace},
		}
		return client.IgnoreNotFound(r.Delete(ctx, pvc))
	}
	deleteSnaps := func() error {
		vs := &snapv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: branch.VSName(b), Namespace: b.Namespace},
		}
		if err := r.Delete(ctx, vs); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		vsc := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: branch.VSCName(b)},
		}
		return client.IgnoreNotFound(r.Delete(ctx, vsc))
	}
	if pinsVolume {
		if err := deletePVC(); err != nil {
			return err
		}
		return deleteSnaps()
	}
	if err := deleteSnaps(); err != nil {
		return err
	}
	return deletePVC()
}

func (r *BranchReconciler) reconcileDelete(ctx context.Context, b *volumesv1alpha1.Branch) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, BranchFinalizer) {
		return ctrl.Result{}, nil
	}
	// The source may already be gone at delete time (source-cascade deletes
	// branches first, but a user can also delete out of order); fall back to
	// the conservative pins=true ordering, which is safe on every backend.
	pins := true
	var src volumesv1alpha1.BranchSource
	if err := r.Get(ctx, client.ObjectKey{Name: b.Spec.Source}, &src); err == nil {
		pins = profile.Resolve(&src).SnapshotPinsVolume
	}
	if err := r.teardownClone(ctx, b, pins); err != nil {
		return ctrl.Result{}, err
	}
	// On-demand teardown leaves a cluster-scoped, unowned VSC — confirm it is
	// actually gone before releasing the finalizer, or it leaks until source
	// teardown. Pool-claimed clones have no per-branch VSC (the seeding pair
	// died at claim time), so the guard does not apply.
	if b.Status.Provisioning != volumesv1alpha1.ProvisionedFromPool {
		var check snapv1.VolumeSnapshotContent
		if err := r.Get(ctx, client.ObjectKey{Name: branch.VSCName(b)}, &check); !apierrors.IsNotFound(err) {
			// Still present (err == nil) or state unknown (transient error):
			// requeue, keep the finalizer.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}
	controllerutil.RemoveFinalizer(b, BranchFinalizer)
	if err := r.Update(ctx, b); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *BranchReconciler) createIfAbsent(ctx context.Context, obj client.Object) error {
	if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// setPhase parks the Branch at Pending with a wait reason, mirrored on the
// Ready condition.
func (r *BranchReconciler) setPhase(ctx context.Context, b *volumesv1alpha1.Branch, reason, msg string) (ctrl.Result, error) {
	condChanged := setCondition(&b.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: b.Generation,
	})
	if condChanged || b.Status.Phase != volumesv1alpha1.BranchPending || b.Status.Message != msg {
		b.Status.Phase = volumesv1alpha1.BranchPending
		b.Status.Message = msg
		if err := r.Status().Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&volumesv1alpha1.Branch{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&snapv1.VolumeSnapshot{}).
		Named("branch").
		Complete(r)
}
