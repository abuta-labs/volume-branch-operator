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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/pool"
)

func newPoolReconciler() *BranchPoolReconciler {
	return &BranchPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ProvisionTimeout: 5 * time.Minute}
}

func reconcilePool(ctx context.Context, name string, times int) {
	reconcilePoolWith(ctx, newPoolReconciler(), name, times)
}

func reconcilePoolWith(ctx context.Context, r *BranchPoolReconciler, name string, times int) {
	for range times {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}
}

func newPool(source string, targetWarm int32, maxWarming *int32) *volumesv1alpha1.BranchPool {
	return &volumesv1alpha1.BranchPool{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName("pool")},
		Spec:       volumesv1alpha1.BranchPoolSpec{Source: source, TargetWarm: targetWarm, MaxWarming: maxWarming},
	}
}

func warmPVCs(ctx context.Context, source string) []corev1.PersistentVolumeClaim {
	var list corev1.PersistentVolumeClaimList
	Expect(k8sClient.List(ctx, &list, client.InNamespace(pool.HoldingNamespace),
		client.MatchingLabels{pool.LabelSource: source, pool.LabelPoolState: pool.StateWarm})).To(Succeed())
	live := list.Items[:0]
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp == nil {
			live = append(live, list.Items[i])
		}
	}
	return live
}

// makeWarmBound fakes what the CSI provisioner + PV binder would do for one
// warm PVC: create a PV for it and mark the PVC Bound. envtest runs neither
// controller, so the hand-off state must be staged.
func makeWarmBound(ctx context.Context, pvc *corev1.PersistentVolumeClaim, scName string) string {
	pvName := uniqueName("pv")
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver, VolumeHandle: pvName},
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              scName,
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "PersistentVolumeClaim",
				Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID,
			},
		},
	}
	Expect(k8sClient.Create(ctx, pv)).To(Succeed())
	pvc.Spec.VolumeName = pvName
	Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
	pvc.Status.Phase = corev1.ClaimBound
	Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	return pvName
}

// readySourceWithSC is readySource, also returning the clone StorageClass
// name (the pool specs need it to build realistic PVs).
func readySourceWithSC(ctx context.Context) (string, string) {
	sc, vsc := uniqueName("sc"), uniqueName("vsc")
	createClasses(ctx, sc, vsc, testDriver)
	name := uniqueName("src")
	Expect(k8sClient.Create(ctx, newSource(name, sc, vsc))).To(Succeed())
	reconcileSource(ctx, name)
	return name, sc
}

var _ = Describe("BranchPool Controller", func() {
	ctx := context.Background()

	It("warms the pool to targetWarm and reports warm once clones bind", func() {
		srcName, sc := readySourceWithSC(ctx)
		bp := newPool(srcName, 2, nil)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 2)

		warms := warmPVCs(ctx, srcName)
		Expect(warms).To(HaveLen(2))
		for i := range warms {
			pc := &warms[i]
			cloneID := pc.Labels[pool.LabelCloneID]
			Expect(cloneID).NotTo(BeEmpty())
			Expect(pc.OwnerReferences).To(HaveLen(1))
			Expect(pc.OwnerReferences[0].Name).To(Equal(bp.Name))
			// Each warm clone has its own seeding VS + VSC pair.
			var vs snapv1.VolumeSnapshot
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "branch-pool-vs-" + cloneID, Namespace: pool.HoldingNamespace}, &vs)).To(Succeed())
			var vsc snapv1.VolumeSnapshotContent
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "branch-pool-vsc-" + cloneID}, &vsc)).To(Succeed())
			Expect(vsc.Spec.DeletionPolicy).To(Equal(snapv1.VolumeSnapshotContentRetain))
		}

		var got volumesv1alpha1.BranchPool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name}, &got)).To(Succeed())
		Expect(got.Status.Warming).To(Equal(int32(2)))
		Expect(got.Status.Warm).To(Equal(int32(0)))

		for i := range warms {
			makeWarmBound(ctx, &warms[i], sc)
		}
		reconcilePool(ctx, bp.Name, 1)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name}, &got)).To(Succeed())
		Expect(got.Status.Warm).To(Equal(int32(2)))
		Expect(got.Status.Warming).To(Equal(int32(0)))
	})

	It("caps concurrent warming at maxWarming", func() {
		srcName, sc := readySourceWithSC(ctx)
		one := int32(1)
		bp := newPool(srcName, 3, &one)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 3) // repeated passes must not exceed the cap

		warms := warmPVCs(ctx, srcName)
		Expect(warms).To(HaveLen(1), "only one clone may warm at a time")

		// The slot frees when the clone binds; the next reconcile starts one more.
		makeWarmBound(ctx, &warms[0], sc)
		reconcilePool(ctx, bp.Name, 1)
		Expect(warmPVCs(ctx, srcName)).To(HaveLen(2))
	})

	It("reclaims warm clones stuck Pending past the provision timeout", func() {
		srcName, _ := readySourceWithSC(ctx)
		bp := newPool(srcName, 1, nil)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 2)
		stuck := warmPVCs(ctx, srcName)
		Expect(stuck).To(HaveLen(1))
		cloneID := stuck[0].Labels[pool.LabelCloneID]

		// A timeout of 1ns makes any Pending clone stale immediately.
		impatient := &BranchPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ProvisionTimeout: time.Nanosecond}
		reconcilePoolWith(ctx, impatient, bp.Name, 1)

		var pvcs corev1.PersistentVolumeClaimList
		Expect(k8sClient.List(ctx, &pvcs, client.InNamespace(pool.HoldingNamespace),
			client.MatchingLabels{pool.LabelCloneID: cloneID})).To(Succeed())
		for i := range pvcs.Items {
			Expect(pvcs.Items[i].DeletionTimestamp).NotTo(BeNil())
		}
		var vsc snapv1.VolumeSnapshotContent
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "branch-pool-vsc-" + cloneID}, &vsc)
		if err == nil {
			Expect(vsc.DeletionTimestamp).NotTo(BeNil())
		}
	})

	It("reaps claimed clones whose claimant Branch no longer exists", func() {
		srcName, sc := readySourceWithSC(ctx)
		bp := newPool(srcName, 1, nil)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 2)
		warms := warmPVCs(ctx, srcName)
		Expect(warms).To(HaveLen(1))
		pvName := makeWarmBound(ctx, &warms[0], sc)

		// Stage a claim by a Branch UID that matches nothing live.
		warms[0].Labels[pool.LabelPoolState] = pool.StateClaimed
		warms[0].Labels[pool.LabelClaimant] = "deadbeef-0000-4000-8000-000000000000"
		Expect(k8sClient.Update(ctx, &warms[0])).To(Succeed())

		reconcilePool(ctx, bp.Name, 1)

		var pvc corev1.PersistentVolumeClaim
		err := k8sClient.Get(ctx, types.NamespacedName{Name: warms[0].Name, Namespace: pool.HoldingNamespace}, &pvc)
		if err == nil {
			Expect(pvc.DeletionTimestamp).NotTo(BeNil())
		}
		var pv corev1.PersistentVolume
		err = k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, &pv)
		if err == nil {
			Expect(pv.DeletionTimestamp).NotTo(BeNil())
		}
	})

	It("tears down unclaimed warm clones when the pool is deleted", func() {
		srcName, _ := readySourceWithSC(ctx)
		bp := newPool(srcName, 2, nil)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 2)
		warms := warmPVCs(ctx, srcName)
		Expect(warms).To(HaveLen(2))

		Expect(k8sClient.Delete(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 2)

		Expect(warmPVCs(ctx, srcName)).To(BeEmpty())
		for i := range warms {
			cloneID := warms[i].Labels[pool.LabelCloneID]
			var vs snapv1.VolumeSnapshot
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "branch-pool-vs-" + cloneID, Namespace: pool.HoldingNamespace}, &vs)
			if err == nil {
				Expect(vs.DeletionTimestamp).NotTo(BeNil())
			}
		}
		var gone volumesv1alpha1.BranchPool
		err := k8sClient.Get(ctx, types.NamespacedName{Name: bp.Name}, &gone)
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		if err == nil {
			Expect(gone.DeletionTimestamp).NotTo(BeNil())
			Expect(gone.Finalizers).NotTo(ContainElement(BranchPoolFinalizer))
		}
	})
})
