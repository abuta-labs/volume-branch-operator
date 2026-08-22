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

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/branch"
	"github.com/arbit-tech/volume-branch-operator/internal/pool"
)

// finishPVCDeletion simulates the pvc-protection controller: envtest's
// apiserver adds the protection finalizer but nothing removes it, so a
// deleted PVC lingers terminating forever unless the test clears it.
func finishPVCDeletion(ctx context.Context, name, ns string) {
	var pvc corev1.PersistentVolumeClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pvc); err != nil {
		return
	}
	if pvc.DeletionTimestamp == nil {
		return
	}
	pvc.Finalizers = nil
	Expect(client.IgnoreNotFound(k8sClient.Update(ctx, &pvc))).To(Succeed())
}

// stageWarmPool creates a pool with one warm, Bound clone and returns
// (poolName, warm PVC name, PV name).
func stageWarmPool(ctx context.Context, srcName, sc string) (string, string, string) {
	bp := newPool(srcName, 1, nil)
	Expect(k8sClient.Create(ctx, bp)).To(Succeed())
	reconcilePool(ctx, bp.Name, 2)
	warms := warmPVCs(ctx, srcName)
	Expect(warms).To(HaveLen(1))
	pvName := makeWarmBound(ctx, &warms[0], sc)
	reconcilePool(ctx, bp.Name, 1) // observe warm=1
	return bp.Name, warms[0].Name, pvName
}

func newBranch(ns, srcName string) *volumesv1alpha1.Branch {
	return &volumesv1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br"), Namespace: ns},
		Spec:       volumesv1alpha1.BranchSpec{Source: srcName, PVCName: uniqueName("clone-pvc")},
	}
}

var _ = Describe("Branch pool-claim fast path", func() {
	ctx := context.Background()
	const ns = "default"

	It("claims a warm clone by rebinding its PV — no new volume objects", func() {
		srcName, sc := readySourceWithSC(ctx)
		poolName, warmName, pvName := stageWarmPool(ctx, srcName, sc)

		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Provisioning).To(Equal(volumesv1alpha1.ProvisionedFromPool))
		// The claim is durably recorded before the warm clone is consumed —
		// the resume anchor once the pool PVC no longer exists.
		Expect(got.Status.ClaimedVolume).To(Equal(pvName))

		// The consumer PVC is statically bound to the pre-warmed PV.
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &pvc)).To(Succeed())
		Expect(pvc.Spec.VolumeName).To(Equal(pvName))
		Expect(pvc.Spec.DataSource).To(BeNil(), "a claim rebinds; it never re-provisions")

		// The PV: Retain across the rebind, ClaimRef pinned to the consumer PVC.
		var pv corev1.PersistentVolume
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, &pv)).To(Succeed())
		Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
		Expect(pv.Spec.ClaimRef).NotTo(BeNil())
		Expect(pv.Spec.ClaimRef.Name).To(Equal(b.Spec.PVCName))
		Expect(pv.Spec.ClaimRef.Namespace).To(Equal(ns))

		// The pool PVC is gone (or terminating), and no per-branch on-demand
		// snapshot objects were created.
		var poolPVC corev1.PersistentVolumeClaim
		err := k8sClient.Get(ctx, types.NamespacedName{Name: warmName, Namespace: pool.HoldingNamespace}, &poolPVC)
		if err == nil {
			Expect(poolPVC.DeletionTimestamp).NotTo(BeNil())
		}
		var vsc snapv1.VolumeSnapshotContent
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: branch.VSCName(&got)}, &vsc))).To(BeTrue())

		// claimedTotal counted the hand-off.
		var gotPool volumesv1alpha1.BranchPool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, &gotPool)).To(Succeed())
		Expect(gotPool.Status.ClaimedTotal).To(Equal(int64(1)))

		bindPVC(ctx, b.Spec.PVCName)
		reconcileBranch(ctx, key, 1)
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchReady))
	})

	It("replenishes the pool after a claim", func() {
		srcName, sc := readySourceWithSC(ctx)
		poolName, _, _ := stageWarmPool(ctx, srcName, sc)
		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		reconcileBranch(ctx, types.NamespacedName{Name: b.Name, Namespace: ns}, 2)

		reconcilePool(ctx, poolName, 1)
		Expect(warmPVCs(ctx, srcName)).To(HaveLen(1), "a fresh clone warms to replace the claimed one")
	})

	It("gives one warm clone to exactly one of two branches; the other goes on-demand", func() {
		srcName, sc := readySourceWithSC(ctx)
		_, _, pvName := stageWarmPool(ctx, srcName, sc)

		a := newBranch(ns, srcName)
		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, a)).To(Succeed())
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		reconcileBranch(ctx, types.NamespacedName{Name: a.Name, Namespace: ns}, 2)
		reconcileBranch(ctx, types.NamespacedName{Name: b.Name, Namespace: ns}, 2)

		var gotA, gotB volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: a.Name, Namespace: ns}, &gotA)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Name, Namespace: ns}, &gotB)).To(Succeed())
		Expect(gotA.Status.Provisioning).To(Equal(volumesv1alpha1.ProvisionedFromPool))
		Expect(gotB.Status.Provisioning).To(Equal(volumesv1alpha1.ProvisionedOnDemand))

		var pvcA corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: a.Spec.PVCName, Namespace: ns}, &pvcA)).To(Succeed())
		Expect(pvcA.Spec.VolumeName).To(Equal(pvName))
		var pvcB corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &pvcB)).To(Succeed())
		Expect(pvcB.Spec.VolumeName).To(BeEmpty(), "the loser provisions its own volume")
		Expect(pvcB.Spec.DataSource).NotTo(BeNil())
	})

	It("keeps the PV reserved for its claimant through the whole rebind (theft hazard)", func() {
		srcName, sc := readySourceWithSC(ctx)
		_, _, pvName := stageWarmPool(ctx, srcName, sc)

		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)

		// Sabotage: delete the consumer PVC mid-rebind. The PV must still name
		// its claimant — an empty ClaimRef here is exactly the theft window.
		Expect(k8sClient.Delete(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: b.Spec.PVCName, Namespace: ns},
		})).To(Succeed())
		var pv corev1.PersistentVolume
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, &pv)).To(Succeed())
		Expect(pv.Spec.ClaimRef).NotTo(BeNil())
		Expect(pv.Spec.ClaimRef.Name).To(Equal(b.Spec.PVCName))

		// The idempotent claim re-creates the consumer PVC on the next pass.
		reconcileBranch(ctx, key, 1)
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &pvc)).To(Succeed())
		Expect(pvc.Spec.VolumeName).To(Equal(pvName))
	})

	It("tears down a pool-claimed branch: consumer PVC and PV both go", func() {
		srcName, sc := readySourceWithSC(ctx)
		_, _, pvName := stageWarmPool(ctx, srcName, sc)
		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)
		bindPVC(ctx, b.Spec.PVCName)
		reconcileBranch(ctx, key, 1)

		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		reconcileBranch(ctx, key, 2)

		var pvc corev1.PersistentVolumeClaim
		err := k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &pvc)
		if err == nil {
			Expect(pvc.DeletionTimestamp).NotTo(BeNil())
		}
		var pv corev1.PersistentVolume
		err = k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, &pv)
		if err == nil {
			Expect(pv.DeletionTimestamp).NotTo(BeNil())
			Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete))
		}
		var gone volumesv1alpha1.Branch
		err = k8sClient.Get(ctx, key, &gone)
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		if err == nil {
			Expect(gone.Finalizers).NotTo(ContainElement(BranchFinalizer))
		}
	})

	It("resets a pool-claimed branch by re-cloning (pool empty -> on-demand)", func() {
		srcName, sc := readySourceWithSC(ctx)
		stageWarmPool(ctx, srcName, sc)
		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)
		bindPVC(ctx, b.Spec.PVCName)
		reconcileBranch(ctx, key, 1)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Provisioning).To(Equal(volumesv1alpha1.ProvisionedFromPool))
		got.Spec.ResetToken = "t1"
		Expect(k8sClient.Update(ctx, &got)).To(Succeed())
		reconcileBranch(ctx, key, 1) // fires the teardown deletes
		finishPVCDeletion(ctx, got.Spec.PVCName, ns)
		reconcileBranch(ctx, key, 2)

		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.ObservedResetToken).To(Equal("t1"))
		// The pool had a single clone, already spent — the re-clone must have
		// gone on-demand with fresh snapshot objects.
		Expect(got.Status.Provisioning).To(Equal(volumesv1alpha1.ProvisionedOnDemand))
		var vsc snapv1.VolumeSnapshotContent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branch.VSCName(&got)}, &vsc)).To(Succeed())
	})

	It("reclaims a claimed-but-unfinished clone when the branch dies in the crash window", func() {
		srcName, sc := readySourceWithSC(ctx)
		_, warmName, pvName := stageWarmPool(ctx, srcName, sc)

		// A Branch that flipped the label but died before anything else
		// persisted: finalizer present, provisioning never recorded.
		b := newBranch(ns, srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		controllerutil.AddFinalizer(b, BranchFinalizer)
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		var warm corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: warmName, Namespace: pool.HoldingNamespace}, &warm)).To(Succeed())
		warm.Labels[pool.LabelPoolState] = pool.StateClaimed
		warm.Labels[pool.LabelClaimant] = string(b.UID)
		Expect(k8sClient.Update(ctx, &warm)).To(Succeed())

		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		reconcileBranch(ctx, types.NamespacedName{Name: b.Name, Namespace: ns}, 2)

		var pvc corev1.PersistentVolumeClaim
		err := k8sClient.Get(ctx, types.NamespacedName{Name: warmName, Namespace: pool.HoldingNamespace}, &pvc)
		if err == nil {
			Expect(pvc.DeletionTimestamp).NotTo(BeNil())
		}
		var pv corev1.PersistentVolume
		err = k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, &pv)
		if err == nil {
			Expect(pv.DeletionTimestamp).NotTo(BeNil())
		}
	})
})
