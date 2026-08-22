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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/pool"
	"github.com/arbit-tech/volume-branch-operator/internal/profile"
)

// BranchPoolFinalizer gates pool deletion on ordered teardown of its warm
// set. Owner references would GC the objects anyway, but in arbitrary order —
// and snapshot-pins-volume backends need the volume gone before its snapshot
// objects.
const BranchPoolFinalizer = "volumes.arbit-tech.com/pool-teardown"

// BranchPoolReconciler keeps targetWarm pre-warmed clones per source and
// reaps clones that went stale.
type BranchPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ProvisionTimeout reaps a warm PVC stuck Pending longer than this —
	// a wedged CSI provisioning would otherwise hold a warming slot forever.
	ProvisionTimeout time.Duration
}

// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branchpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branchpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branchpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branchsources,verbs=get;list;watch
// +kubebuilder:rbac:groups=volumes.arbit-tech.com,resources=branches,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;update;patch;delete

func randCloneID() string { return rand.String(8) }

// Reconcile drives the warm set toward spec.targetWarm, bounded by
// spec.maxWarming concurrent creations, and keeps status.warm/warming
// honest. A periodic requeue drives replenishment and the reapers.
func (r *BranchPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var bp volumesv1alpha1.BranchPool
	if err := r.Get(ctx, req.NamespacedName, &bp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !bp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &bp)
	}
	if controllerutil.AddFinalizer(&bp, BranchPoolFinalizer) {
		if err := r.Update(ctx, &bp); err != nil {
			return ctrl.Result{}, err
		}
	}

	var src volumesv1alpha1.BranchSource
	if err := r.Get(ctx, client.ObjectKey{Name: bp.Spec.Source}, &src); err != nil || src.Status.Phase != volumesv1alpha1.BranchSourceReady {
		// No Ready source: nothing to warm against; keep whatever exists.
		return r.writeStatus(ctx, &bp, 0, 0)
	}

	if err := r.ensureHoldingNamespace(ctx); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reapStaleWarming(ctx, bp.Spec.Source); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reapOrphanClaims(ctx, bp.Spec.Source); err != nil {
		return ctrl.Result{}, err
	}

	warm, warming, err := r.inventory(ctx, bp.Spec.Source)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Replenish: bring warm+warming up to targetWarm, without ever exceeding
	// maxWarming concurrent creations. Unset maxWarming resolves from the
	// source's substrate profile — backends that serialize CreateVolume get
	// a low cap, fast-clone backends a wide one.
	prof := profile.Resolve(&src)
	if prof.NeedsSize() && src.Status.SizeBytes == 0 {
		// Same rule as the Branch path: actual-mode sizing must not guess.
		// The source's probe is discovering the size; warm once it lands.
		if _, _, err := r.inventory(ctx, bp.Spec.Source); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	maxWarming := prof.MaxWarmingDefault
	if bp.Spec.MaxWarming != nil {
		maxWarming = *bp.Spec.MaxWarming
	}
	need := bp.Spec.TargetWarm - (warm + warming)
	budget := maxWarming - warming
	toCreate := min(need, budget)
	for range toCreate {
		if err := r.createWarmClone(ctx, &bp, &src, prof.SizeRequest(src.Status.SizeBytes)); err != nil {
			return ctrl.Result{}, err
		}
		warming++
	}

	return r.writeStatus(ctx, &bp, warm, warming)
}

func (r *BranchPoolReconciler) ensureHoldingNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: pool.HoldingNamespace}, ns); err == nil {
		return nil
	} else if client.IgnoreNotFound(err) != nil {
		return err
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: pool.HoldingNamespace}}
	return client.IgnoreAlreadyExists(r.Create(ctx, ns))
}

// inventory counts warm (Bound) and warming (not yet Bound) clones for a
// source. Claimed clones are transient hand-off state and counted by neither.
func (r *BranchPoolReconciler) inventory(ctx context.Context, source string) (warm, warming int32, err error) {
	var list corev1.PersistentVolumeClaimList
	if err = r.List(ctx, &list, client.InNamespace(pool.HoldingNamespace),
		client.MatchingLabels{pool.LabelSource: source, pool.LabelPoolState: pool.StateWarm}); err != nil {
		return
	}
	for i := range list.Items {
		pc := &list.Items[i]
		if pc.DeletionTimestamp != nil {
			continue
		}
		if pc.Status.Phase == corev1.ClaimBound {
			warm++
		} else {
			warming++
		}
	}
	return
}

func (r *BranchPoolReconciler) createWarmClone(ctx context.Context, bp *volumesv1alpha1.BranchPool, src *volumesv1alpha1.BranchSource, size resource.Quantity) error {
	set := pool.BuildWarmSet(bp, src, randCloneID(), size)
	if err := r.Create(ctx, set.VSC); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	if err := r.Create(ctx, set.VS); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	if err := r.Create(ctx, set.PVC); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

// reapStaleWarming reclaims warm PVCs stuck Pending past ProvisionTimeout;
// the replenish loop recreates their capacity.
func (r *BranchPoolReconciler) reapStaleWarming(ctx context.Context, source string) error {
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list, client.InNamespace(pool.HoldingNamespace),
		client.MatchingLabels{pool.LabelSource: source, pool.LabelPoolState: pool.StateWarm}); err != nil {
		return err
	}
	for i := range list.Items {
		pc := &list.Items[i]
		if pc.Status.Phase == corev1.ClaimBound || pc.DeletionTimestamp != nil {
			continue
		}
		if time.Since(pc.CreationTimestamp.Time) < r.ProvisionTimeout {
			continue
		}
		if err := pool.ReclaimCloneSet(ctx, r.Client, pc); err != nil {
			return err
		}
	}
	return nil
}

// reapOrphanClaims reclaims claimed clones whose claimant Branch no longer
// exists — a claimant that died between the label flip and its consumer PVC
// would otherwise leak the volume forever.
func (r *BranchPoolReconciler) reapOrphanClaims(ctx context.Context, source string) error {
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list, client.InNamespace(pool.HoldingNamespace),
		client.MatchingLabels{pool.LabelSource: source, pool.LabelPoolState: pool.StateClaimed}); err != nil {
		return err
	}
	if len(list.Items) == 0 {
		return nil
	}
	var branches volumesv1alpha1.BranchList
	if err := r.List(ctx, &branches); err != nil {
		return err
	}
	live := map[string]bool{}
	for i := range branches.Items {
		live[string(branches.Items[i].UID)] = true
	}
	for i := range list.Items {
		pc := &list.Items[i]
		if pc.DeletionTimestamp != nil {
			continue
		}
		if claimant := pc.Labels[pool.LabelClaimant]; claimant == "" || live[claimant] {
			continue
		}
		if err := pool.ReclaimCloneSet(ctx, r.Client, pc); err != nil {
			return err
		}
	}
	return nil
}

func (r *BranchPoolReconciler) reconcileDelete(ctx context.Context, bp *volumesv1alpha1.BranchPool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(bp, BranchPoolFinalizer) {
		return ctrl.Result{}, nil
	}
	// Ordered teardown of the unclaimed warm set: each clone's PVC first
	// (its PV is Delete-policy while warm, so the CSI driver destroys the
	// volume), then the spent snapshot pair. Claimed clones belong to their
	// Branch now and are left alone.
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list, client.InNamespace(pool.HoldingNamespace),
		client.MatchingLabels{pool.LabelSource: bp.Spec.Source, pool.LabelPoolState: pool.StateWarm}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range list.Items {
		if err := pool.ReclaimCloneSet(ctx, r.Client, &list.Items[i]); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(bp, BranchPoolFinalizer)
	if err := r.Update(ctx, bp); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *BranchPoolReconciler) writeStatus(ctx context.Context, bp *volumesv1alpha1.BranchPool, warm, warming int32) (ctrl.Result, error) {
	if bp.Status.Warm != warm || bp.Status.Warming != warming {
		bp.Status.Warm = warm
		bp.Status.Warming = warming
		if err := r.Status().Update(ctx, bp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// SetupWithManager wires the controller; a PVC in the holding namespace maps
// back to the pools of its source, so claims trigger replenishment promptly.
func (r *BranchPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.ProvisionTimeout == 0 {
		r.ProvisionTimeout = 5 * time.Minute
	}
	mapPVC := func(ctx context.Context, obj client.Object) []reconcile.Request {
		if obj.GetNamespace() != pool.HoldingNamespace {
			return nil
		}
		source := obj.GetLabels()[pool.LabelSource]
		if source == "" {
			return nil
		}
		var pools volumesv1alpha1.BranchPoolList
		if err := r.List(ctx, &pools); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range pools.Items {
			if pools.Items[i].Spec.Source == source {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Name: pools.Items[i].Name}})
			}
		}
		return reqs
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&volumesv1alpha1.BranchPool{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(mapPVC)).
		Named("branchpool").
		Complete(r)
}
