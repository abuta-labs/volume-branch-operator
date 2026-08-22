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

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
)

const (
	testDriver = "zfs.csi.openebs.io"
	testSC     = "branch-clone"
	testVSC    = "branch-snap"
)

var seq = 0

// uniqueName returns a per-spec unique object name: cluster-scoped fixtures
// must not collide across specs that run in the same envtest instance.
func uniqueName(prefix string) string {
	seq++
	return fmt.Sprintf("%s-%d", prefix, seq)
}

func newSourceReconciler() *BranchSourceReconciler {
	return &BranchSourceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
}

func createClasses(ctx context.Context, scName, vscName, driver string) {
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: scName},
		Provisioner: driver,
	}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, sc))).To(Succeed())
	vsc := &snapv1.VolumeSnapshotClass{
		ObjectMeta:     metav1.ObjectMeta{Name: vscName},
		Driver:         driver,
		DeletionPolicy: snapv1.VolumeSnapshotContentRetain,
	}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, vsc))).To(Succeed())
}

func newSource(name, scName, vscName string) *volumesv1alpha1.BranchSource {
	return &volumesv1alpha1.BranchSource{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: volumesv1alpha1.BranchSourceSpec{
			SnapshotHandle:          "snap-" + name,
			CSIDriver:               testDriver,
			CloneStorageClassName:   scName,
			VolumeSnapshotClassName: vscName,
		},
	}
}

// reconcileSource drains one source's reconcile (finalizer add + status pass).
func reconcileSource(ctx context.Context, name string) {
	r := newSourceReconciler()
	for range 3 {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}
}

var _ = Describe("BranchSource Controller", func() {
	ctx := context.Background()

	It("marks a source Ready when its classes exist and match the driver", func() {
		sc, vsc := uniqueName("sc"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, testDriver)
		name := uniqueName("src-ready")
		Expect(k8sClient.Create(ctx, newSource(name, sc, vsc))).To(Succeed())
		reconcileSource(ctx, name)

		var got volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchSourceReady))
		Expect(got.Status.Message).To(BeEmpty())
		Expect(got.Finalizers).To(ContainElement(SourceFinalizer))
	})

	It("marks a source Invalid when the StorageClass is missing", func() {
		name := uniqueName("src-nosc")
		Expect(k8sClient.Create(ctx, newSource(name, uniqueName("absent"), testVSC))).To(Succeed())
		reconcileSource(ctx, name)

		var got volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchSourceInvalid))
		Expect(got.Status.Message).To(ContainSubstring("not found"))
	})

	It("marks a source Invalid when the StorageClass provisioner mismatches csiDriver", func() {
		sc, vsc := uniqueName("sc-wrong"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, "other.csi.example.com")
		name := uniqueName("src-mismatch")
		src := newSource(name, sc, vsc)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		reconcileSource(ctx, name)

		var got volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchSourceInvalid))
		Expect(got.Status.Message).To(ContainSubstring("provisioned by"))
	})

	It("cascades deletion to Branches and only then releases its finalizer", func() {
		sc, vsc := uniqueName("sc"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, testDriver)
		name := uniqueName("src-cascade")
		Expect(k8sClient.Create(ctx, newSource(name, sc, vsc))).To(Succeed())
		reconcileSource(ctx, name)

		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("child"), Namespace: "default"},
			Spec:       volumesv1alpha1.BranchSpec{Source: name, PVCName: uniqueName("pvc")},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		// Give the child its finalizer so the cascade has something to wait on.
		br := &BranchReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := br.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace}})
		Expect(err).NotTo(HaveOccurred())

		var src volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &src)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &src)).To(Succeed())

		// First delete pass: cascade issued, finalizer retained, source still present.
		r := newSourceReconciler()
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).NotTo(BeZero())
		var childAfter volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Name, Namespace: b.Namespace}, &childAfter)).To(Succeed())
		Expect(childAfter.DeletionTimestamp.IsZero()).To(BeFalse())

		// Let the child's own finalizer run to completion.
		for range 3 {
			_, err = br.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace}})
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: b.Name, Namespace: b.Namespace}, &volumesv1alpha1.Branch{}))
		}).Should(BeTrue())

		// Subsequent passes finish the cascade and release the source.
		for range 3 {
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &volumesv1alpha1.BranchSource{}))
		}).Should(BeTrue())
	})
})
