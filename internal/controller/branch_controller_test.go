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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/branch"
)

func newBranchReconciler() *BranchReconciler {
	return &BranchReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
}

// readySource creates classes + a source and reconciles it to Ready.
func readySource(ctx context.Context) string {
	sc, vsc := uniqueName("sc"), uniqueName("vsc")
	createClasses(ctx, sc, vsc, testDriver)
	name := uniqueName("src")
	Expect(k8sClient.Create(ctx, newSource(name, sc, vsc))).To(Succeed())
	reconcileSource(ctx, name)
	return name
}

func reconcileBranch(ctx context.Context, key types.NamespacedName, times int) {
	r := newBranchReconciler()
	for range times {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
}

// bindPVC simulates the CSI provisioner: flips the PVC's status to Bound.
// envtest runs no provisioner, so the bind must be faked to reach Ready.
func bindPVC(ctx context.Context, name, ns string) {
	var pvc corev1.PersistentVolumeClaim
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pvc)).To(Succeed())
	pvc.Status.Phase = corev1.ClaimBound
	Expect(k8sClient.Status().Update(ctx, &pvc)).To(Succeed())
}

var _ = Describe("Branch Controller", func() {
	ctx := context.Background()
	const ns = "default"

	It("creates the on-demand clone set and reaches Ready when the PVC binds", func() {
		srcName := readySource(ctx)
		var src volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: srcName}, &src)).To(Succeed())
		src.Status.SizeBytes = 42
		Expect(k8sClient.Status().Update(ctx, &src)).To(Succeed())

		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br"), Namespace: ns},
			Spec:       volumesv1alpha1.BranchSpec{Source: srcName, PVCName: uniqueName("clone-pvc")},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchCloning))
		Expect(got.Status.Provisioning).To(Equal(volumesv1alpha1.ProvisionedOnDemand))
		Expect(got.Finalizers).To(ContainElement(BranchFinalizer))

		// The VSC: cluster-scoped, Retain, right handle, bound to the VS.
		var vsc snapv1.VolumeSnapshotContent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branch.VSCName(&got)}, &vsc)).To(Succeed())
		Expect(vsc.Spec.DeletionPolicy).To(Equal(snapv1.VolumeSnapshotContentRetain))
		Expect(*vsc.Spec.Source.SnapshotHandle).To(Equal("snap-" + srcName))
		Expect(vsc.Spec.VolumeSnapshotRef.Name).To(Equal(branch.VSName(&got)))
		Expect(vsc.Spec.VolumeSnapshotRef.Namespace).To(Equal(ns))
		Expect(vsc.Labels[branch.SourceLabel]).To(Equal(srcName))

		// The VS: owned by the Branch, statically bound to the VSC.
		var vs snapv1.VolumeSnapshot
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branch.VSName(&got), Namespace: ns}, &vs)).To(Succeed())
		Expect(*vs.Spec.Source.VolumeSnapshotContentName).To(Equal(branch.VSCName(&got)))
		Expect(vs.OwnerReferences).To(HaveLen(1))
		Expect(vs.OwnerReferences[0].Name).To(Equal(got.Name))

		// The PVC: consumer-named, snapshot dataSource, owned by the Branch.
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &pvc)).To(Succeed())
		Expect(pvc.Spec.DataSource.Kind).To(Equal("VolumeSnapshot"))
		Expect(pvc.Spec.DataSource.Name).To(Equal(branch.VSName(&got)))
		Expect(pvc.OwnerReferences).To(HaveLen(1))

		bindPVC(ctx, b.Spec.PVCName, ns)
		reconcileBranch(ctx, key, 1)
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchReady))
		Expect(got.Status.ClonedBytes).To(Equal(int64(42)))
	})

	It("stays Pending while the source is missing or not Ready", func() {
		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br-nosrc"), Namespace: ns},
			Spec:       volumesv1alpha1.BranchSpec{Source: uniqueName("ghost"), PVCName: uniqueName("pvc")},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchPending))
		Expect(got.Status.Message).To(ContainSubstring("not found"))
		// No clone objects were created.
		Expect(apierrors.IsNotFound(
			k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &corev1.PersistentVolumeClaim{}),
		)).To(BeTrue())
	})

	It("re-clones when resetToken changes, recording the observed token", func() {
		srcName := readySource(ctx)
		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br-reset"), Namespace: ns},
			Spec:       volumesv1alpha1.BranchSpec{Source: srcName, PVCName: uniqueName("pvc"), ResetToken: "t1"},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)
		bindPVC(ctx, b.Spec.PVCName, ns)
		reconcileBranch(ctx, key, 1)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchReady))
		Expect(got.Status.ObservedResetToken).To(Equal("t1"))

		got.Spec.ResetToken = "t2"
		Expect(k8sClient.Update(ctx, &got)).To(Succeed())
		reconcileBranch(ctx, key, 1) // reset pass: teardown + Cloning

		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.ObservedResetToken).To(Equal("t2"))
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchCloning))
		Expect(got.Status.Provisioning).To(BeEmpty())

		// The old clone objects were deleted (or are terminating).
		var vs snapv1.VolumeSnapshot
		err := k8sClient.Get(ctx, types.NamespacedName{Name: branch.VSName(&got), Namespace: ns}, &vs)
		if err == nil {
			Expect(vs.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	It("resets a branch born without a token when its first token arrives", func() {
		srcName := readySource(ctx)
		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br-firsttoken"), Namespace: ns},
			Spec:       volumesv1alpha1.BranchSpec{Source: srcName, PVCName: uniqueName("pvc")},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)
		bindPVC(ctx, b.Spec.PVCName, ns)
		reconcileBranch(ctx, key, 1)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchReady))
		Expect(got.Status.ObservedResetToken).To(BeEmpty())

		// "" -> "t1": the first real token must trigger a full reset, not be
		// swallowed as a baseline observation.
		got.Spec.ResetToken = "t1"
		Expect(k8sClient.Update(ctx, &got)).To(Succeed())
		reconcileBranch(ctx, key, 1)

		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.ObservedResetToken).To(Equal("t1"))
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchCloning))
		Expect(got.Status.Provisioning).To(BeEmpty())
		var vs snapv1.VolumeSnapshot
		err := k8sClient.Get(ctx, types.NamespacedName{Name: branch.VSName(&got), Namespace: ns}, &vs)
		if err == nil {
			Expect(vs.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	It("sets expiresAt from spec.ttl at Ready and reaps at expiry", func() {
		srcName := readySource(ctx)
		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br-ttl"), Namespace: ns},
			Spec: volumesv1alpha1.BranchSpec{
				Source:  srcName,
				PVCName: uniqueName("pvc"),
				TTL:     &metav1.Duration{Duration: 50 * time.Millisecond},
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)
		bindPVC(ctx, b.Spec.PVCName, ns)
		reconcileBranch(ctx, key, 1)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchReady))
		Expect(got.Status.ExpiresAt).NotTo(BeNil())

		time.Sleep(60 * time.Millisecond)
		reconcileBranch(ctx, key, 3) // reap pass + finalizer teardown passes
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &volumesv1alpha1.Branch{}))
		}, 5*time.Second).Should(BeTrue())
	})

	It("tears down PVC before snapshot objects and confirms the VSC is gone before releasing", func() {
		srcName := readySource(ctx)
		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("br-del"), Namespace: ns},
			Spec:       volumesv1alpha1.BranchSpec{Source: srcName, PVCName: uniqueName("pvc")},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: ns}
		reconcileBranch(ctx, key, 2)

		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		vscName := branch.VSCName(&got)

		Expect(k8sClient.Delete(ctx, &got)).To(Succeed())
		reconcileBranch(ctx, key, 3)

		// All clone objects gone (or terminating, for the PVC, whose
		// protection finalizer has no controller to clear it in envtest).
		var pvc corev1.PersistentVolumeClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: ns}, &pvc); err == nil {
			Expect(pvc.DeletionTimestamp.IsZero()).To(BeFalse())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
		Expect(apierrors.IsNotFound(
			k8sClient.Get(ctx, types.NamespacedName{Name: vscName}, &snapv1.VolumeSnapshotContent{}),
		)).To(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &volumesv1alpha1.Branch{}))
		}, 5*time.Second).Should(BeTrue())
	})
})
