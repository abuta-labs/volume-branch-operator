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

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/branch"
)

// BranchFinalizer gates Branch deletion on clone teardown: PVC, then
// VolumeSnapshot, then VolumeSnapshotContent, strictly in that order.
const BranchFinalizer = "volumes.arbit-tech.com/branch-teardown"

// BranchReconciler reconciles a Branch object.
type BranchReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branchsources,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots;volumesnapshotcontents,verbs=get;list;watch;create;delete

// Reconcile drives a Branch to a Bound PVC: on-demand VSC + VS + PVC from the
// source's snapshot handle. (The pool fast path lands in a later slice; every
// Branch currently provisions on demand.)
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
			return r.setPhase(ctx, &b, volumesv1alpha1.BranchPending, "BranchSource not found")
		}
		return ctrl.Result{}, err
	}
	if src.Status.Phase != volumesv1alpha1.BranchSourceReady {
		return r.setPhase(ctx, &b, volumesv1alpha1.BranchPending, "waiting for BranchSource to become Ready")
	}

	// Reset: a changed token discards the current clone and re-runs the flow.
	// Baseline vs reset is decided by whether a clone exists to tear down
	// (status.provisioning set), never by token values: an empty token is a
	// legitimate baseline, a branch born without a token must still reset
	// when its first real token arrives ("" -> "t1"), and so must "t1" -> "".
	if b.Status.Provisioning == "" {
		b.Status.ObservedResetToken = b.Spec.ResetToken
	} else if b.Status.ObservedResetToken != b.Spec.ResetToken {
		if err := r.teardownClone(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
		b.Status.ObservedResetToken = b.Spec.ResetToken
		b.Status.Phase = volumesv1alpha1.BranchCloning
		b.Status.Provisioning = ""
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
	} else {
		b.Status.Phase = volumesv1alpha1.BranchCloning
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

// ensureClone creates the on-demand clone object set (create-or-ignore-exists):
// per-branch VolumeSnapshotContent + VolumeSnapshot + the consumer-named PVC.
func (r *BranchReconciler) ensureClone(ctx context.Context, b *volumesv1alpha1.Branch, src *volumesv1alpha1.BranchSource) error {
	if b.Status.Provisioning == "" {
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
	pvc := branch.BuildPVC(b, src)
	if err := controllerutil.SetControllerReference(b, pvc, r.Scheme); err != nil {
		return err
	}
	return r.createIfAbsent(ctx, pvc)
}

// teardownClone deletes the clone object set in dependency order: PVC first
// (the volume), then VolumeSnapshot, then VolumeSnapshotContent. On backends
// where a snapshot pins its parent volume, deleting snapshot objects before
// the volume objects can wedge the backend-side cleanup — the volume must go
// first. Deletes are fired here; completion is confirmed by reconcileDelete.
func (r *BranchReconciler) teardownClone(ctx context.Context, b *volumesv1alpha1.Branch) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: b.Spec.PVCName, Namespace: b.Namespace},
	}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	vs := &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: branch.VSName(b), Namespace: b.Namespace},
	}
	if err := r.Delete(ctx, vs); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	vsc := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: branch.VSCName(b)},
	}
	if err := r.Delete(ctx, vsc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *BranchReconciler) reconcileDelete(ctx context.Context, b *volumesv1alpha1.Branch) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, BranchFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.teardownClone(ctx, b); err != nil {
		return ctrl.Result{}, err
	}
	// The VSC is cluster-scoped and unowned — confirm it is actually gone
	// before releasing the finalizer, or it leaks until source teardown.
	var check snapv1.VolumeSnapshotContent
	if err := r.Get(ctx, client.ObjectKey{Name: branch.VSCName(b)}, &check); !apierrors.IsNotFound(err) {
		// Still present (err == nil) or state unknown (transient error):
		// requeue, keep the finalizer.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
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

func (r *BranchReconciler) setPhase(ctx context.Context, b *volumesv1alpha1.Branch, phase volumesv1alpha1.BranchPhase, msg string) (ctrl.Result, error) {
	if b.Status.Phase != phase || b.Status.Message != msg {
		b.Status.Phase = phase
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
