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
	"fmt"
	"time"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	volumesv1alpha1 "github.com/abuta-labs/volume-branch-operator/api/v1alpha1"
	"github.com/abuta-labs/volume-branch-operator/internal/branch"
	"github.com/abuta-labs/volume-branch-operator/internal/profile"
)

// SourceFinalizer gates BranchSource deletion on the cascade: every Branch of
// the source is deleted first (each tears down its own clone objects via its
// own finalizer), then any engine-created cluster-scoped
// VolumeSnapshotContent that outlived its Branch is swept.
const SourceFinalizer = "volumes.abuta-labs.com/source-teardown"

// BranchSourceReconciler reconciles a BranchSource object.
type BranchSourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branchsources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branchsources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branchsources/finalizers,verbs=update
// +kubebuilder:rbac:groups=volumes.abuta-labs.com,resources=branches,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents,verbs=get;list;watch;create;delete

// Reconcile validates the source against the cluster's storage configuration
// and moves status.phase to Ready or Invalid accordingly.
func (r *BranchSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var src volumesv1alpha1.BranchSource
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !src.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &src)
	}
	if controllerutil.AddFinalizer(&src, SourceFinalizer) {
		if err := r.Update(ctx, &src); err != nil {
			return ctrl.Result{}, err
		}
	}

	phase, reason, msg := r.validate(ctx, &src)
	prof := profile.Resolve(&src)
	resolved := prof.Resolved()

	// Size discovery. The source is defined by a bare snapshot handle, which
	// carries no size, but actual-mode sizing may not proceed without one
	// (profile.NeedsSize). Order: an explicit spec.sizeBytes declaration
	// wins; otherwise scan VolumeSnapshotContents for one whose
	// snapshotHandle matches and read the sidecar's restoreSize off it —
	// the snapshot's originating (dynamically provisioned) content carries
	// it. A statically bound content does NOT (sidecars fill restoreSize at
	// cut time only — verified on zfs-localpv), which is why the engine
	// cannot mint its own probe object and must find an original.
	// Discovered once, the size sticks.
	sizeBytes := src.Status.SizeBytes
	if src.Spec.SizeBytes > 0 {
		sizeBytes = src.Spec.SizeBytes
	}
	if sizeBytes == 0 && phase == volumesv1alpha1.BranchSourceReady && prof.NeedsSize() {
		var err error
		if sizeBytes, err = r.discoverSizeBytes(ctx, &src); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The Ready condition is stricter than phase: a validated source that is
	// still holding for size discovery (actual-mode sizing) validates fine
	// but cannot produce clones yet, and consumers watching conditions should
	// see that as not-Ready with a reason naming the way out.
	sizeHolding := phase == volumesv1alpha1.BranchSourceReady && prof.NeedsSize() && sizeBytes == 0
	cond := metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             volumesv1alpha1.ReasonValidated,
		Message:            "clones can be created from this source",
		ObservedGeneration: src.Generation,
	}
	switch {
	case phase == volumesv1alpha1.BranchSourcePending:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = volumesv1alpha1.ReasonValidationError
		cond.Message = msg
	case phase == volumesv1alpha1.BranchSourceInvalid:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reason
		cond.Message = msg
	case sizeHolding:
		cond.Status = metav1.ConditionFalse
		cond.Reason = volumesv1alpha1.ReasonSizeUnknown
		cond.Message = "actual-mode sizing is holding clone creation: no VolumeSnapshotContent reports a restoreSize for this snapshotHandle — keep the snapshot's originating content, or declare spec.sizeBytes"
	}

	condChanged := setCondition(&src.Status.Conditions, cond)
	if condChanged || src.Status.Phase != phase || src.Status.Message != msg ||
		src.Status.SizeBytes != sizeBytes ||
		!apiequality.Semantic.DeepEqual(src.Status.ResolvedProfile, resolved) {
		src.Status.Phase = phase
		src.Status.Message = msg
		src.Status.SizeBytes = sizeBytes
		src.Status.ResolvedProfile = resolved
		if err := r.Status().Update(ctx, &src); err != nil {
			return ctrl.Result{}, err
		}
	}
	if phase != volumesv1alpha1.BranchSourceReady {
		// The missing class may be created later; poll rather than wedge.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if sizeHolding {
		// Clone creation is holding on the size; keep scanning.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// discoverSizeBytes scans VolumeSnapshotContents for one whose
// snapshotHandle matches the source's and reports a restore size — in the
// common flow, the snapshot's originating content. Returns 0 when none does
// (the operator then either recreates/keeps that content or declares
// spec.sizeBytes).
func (r *BranchSourceReconciler) discoverSizeBytes(ctx context.Context, src *volumesv1alpha1.BranchSource) (int64, error) {
	var vscs snapv1.VolumeSnapshotContentList
	if err := r.List(ctx, &vscs); err != nil {
		return 0, err
	}
	for i := range vscs.Items {
		st := vscs.Items[i].Status
		if st == nil || st.SnapshotHandle == nil || *st.SnapshotHandle != src.Spec.SnapshotHandle {
			continue
		}
		if st.RestoreSize != nil && *st.RestoreSize > 0 {
			return *st.RestoreSize, nil
		}
	}
	return 0, nil
}

// validate checks that the referenced classes exist and are mutually
// consistent: both must belong to the CSI driver that owns the snapshot
// handle, or clone PVCs would be provisioned by a driver that has never
// heard of the snapshot. The returned reason feeds the Ready condition.
func (r *BranchSourceReconciler) validate(ctx context.Context, src *volumesv1alpha1.BranchSource) (volumesv1alpha1.BranchSourcePhase, string, string) {
	var sc storagev1.StorageClass
	if err := r.Get(ctx, client.ObjectKey{Name: src.Spec.CloneStorageClassName}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return volumesv1alpha1.BranchSourceInvalid, volumesv1alpha1.ReasonClassMissing,
				fmt.Sprintf("StorageClass %q not found", src.Spec.CloneStorageClassName)
		}
		return volumesv1alpha1.BranchSourcePending, volumesv1alpha1.ReasonValidationError, err.Error()
	}
	if sc.Provisioner != src.Spec.CSIDriver {
		return volumesv1alpha1.BranchSourceInvalid, volumesv1alpha1.ReasonDriverMismatch,
			fmt.Sprintf("StorageClass %q is provisioned by %q, not spec.csiDriver %q",
				sc.Name, sc.Provisioner, src.Spec.CSIDriver)
	}
	var vsc snapv1.VolumeSnapshotClass
	if err := r.Get(ctx, client.ObjectKey{Name: src.Spec.VolumeSnapshotClassName}, &vsc); err != nil {
		if apierrors.IsNotFound(err) {
			return volumesv1alpha1.BranchSourceInvalid, volumesv1alpha1.ReasonClassMissing,
				fmt.Sprintf("VolumeSnapshotClass %q not found", src.Spec.VolumeSnapshotClassName)
		}
		return volumesv1alpha1.BranchSourcePending, volumesv1alpha1.ReasonValidationError, err.Error()
	}
	if vsc.Driver != src.Spec.CSIDriver {
		return volumesv1alpha1.BranchSourceInvalid, volumesv1alpha1.ReasonDriverMismatch,
			fmt.Sprintf("VolumeSnapshotClass %q belongs to driver %q, not spec.csiDriver %q",
				vsc.Name, vsc.Driver, src.Spec.CSIDriver)
	}
	return volumesv1alpha1.BranchSourceReady, volumesv1alpha1.ReasonValidated, ""
}

// reconcileDelete cascades the source delete to its Branches, then sweeps any
// engine-labeled VolumeSnapshotContent left behind (cluster-scoped objects
// cannot carry an owner reference to a namespaced Branch, so garbage
// collection never reaps them). All per-branch VSCs are Retain-policy, so the
// sweep detaches Kubernetes objects only — the physical snapshot is
// deliberately untouched here; physical reclaim is a separate, source-level
// operation out of this controller's scope.
func (r *BranchSourceReconciler) reconcileDelete(ctx context.Context, src *volumesv1alpha1.BranchSource) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(src, SourceFinalizer) {
		return ctrl.Result{}, nil
	}

	// Cascade: delete every Branch referencing this source. Filtered in code
	// rather than by field index — a source has tens of branches, not
	// thousands, and reconcilers here are also invoked directly in tests
	// where no index-backed cache exists.
	var branches volumesv1alpha1.BranchList
	if err := r.List(ctx, &branches); err != nil {
		return ctrl.Result{}, err
	}
	remaining := 0
	for i := range branches.Items {
		b := &branches.Items[i]
		if b.Spec.Source != src.Name {
			continue
		}
		remaining++
		if b.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, b); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	var vscs snapv1.VolumeSnapshotContentList
	if err := r.List(ctx, &vscs, client.MatchingLabels{branch.SourceLabel: src.Name}); err != nil {
		return ctrl.Result{}, err
	}
	pending := 0
	for i := range vscs.Items {
		v := &vscs.Items[i]
		if !v.DeletionTimestamp.IsZero() {
			continue
		}
		pending++
		if err := r.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if pending > 0 {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(src, SourceFinalizer)
	if err := r.Update(ctx, src); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BranchSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&volumesv1alpha1.BranchSource{}).
		Named("branchsource").
		Complete(r)
}
