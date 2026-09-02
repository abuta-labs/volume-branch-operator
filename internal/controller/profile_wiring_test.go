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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	volumesv1alpha1 "github.com/abuta-labs/volume-branch-operator/api/v1alpha1"
	"github.com/abuta-labs/volume-branch-operator/internal/pool"
)

// The substrate profile wires driver-specific policy through the
// controllers: clone sizing, warming caps, teardown ordering.
var _ = Describe("Substrate profile wiring", func() {
	ctx := context.Background()

	It("publishes the resolved profile on BranchSource status", func() {
		srcName := readySource(ctx) // testDriver = zfs.csi.openebs.io
		var src volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: srcName}, &src)).To(Succeed())
		Expect(src.Status.ResolvedProfile).NotTo(BeNil())
		Expect(src.Status.ResolvedProfile.CloneSizeMode).To(Equal(volumesv1alpha1.CloneSizeModeActual))
		Expect(src.Status.ResolvedProfile.MaxWarmingDefault).To(Equal(int32(8)))
		Expect(src.Status.ResolvedProfile.SnapshotPinsVolume).To(BeFalse())
	})

	It("applies spec.profile overrides over the builtin", func() {
		sc, vsc := uniqueName("sc"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, testDriver)
		src := newSource(uniqueName("src"), sc, vsc)
		mode := volumesv1alpha1.CloneSizeModeSentinel
		src.Spec.Profile = &volumesv1alpha1.ProfileOverrides{
			CloneSizeMode:     &mode,
			MaxWarmingDefault: ptr.To(int32(3)),
		}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		reconcileSource(ctx, src.Name)

		var got volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: src.Name}, &got)).To(Succeed())
		Expect(got.Status.ResolvedProfile.CloneSizeMode).To(Equal(volumesv1alpha1.CloneSizeModeSentinel))
		Expect(got.Status.ResolvedProfile.MaxWarmingDefault).To(Equal(int32(3)))
	})

	It("sizes on-demand clone PVCs from the source size in actual mode", func() {
		srcName := readySource(ctx)
		// Hand the source a discovered size (in a real cluster this comes
		// from the first clone VSC's restoreSize).
		var src volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: srcName}, &src)).To(Succeed())
		src.Status.SizeBytes = 5 << 30 // 5Gi
		Expect(k8sClient.Status().Update(ctx, &src)).To(Succeed())

		b := newBranch(srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		reconcileBranch(ctx, types.NamespacedName{Name: b.Name, Namespace: "default"}, 2)

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: "default"}, &pvc)).To(Succeed())
		q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(q.Value()).To(Equal(int64(5<<30)), "actual mode must request the source's size")
	})

	It("holds clone creation while an actual-mode source size is unknown", func() {
		// A sentinel-sized guess below the snapshot's restore size is refused
		// by size-enforcing drivers and the PVC wedges permanently — so the
		// engine must WAIT, not guess.
		sc, vsc := uniqueName("sc"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, testDriver)
		src := newSource(uniqueName("src"), sc, vsc)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		reconcileSource(ctx, src.Name) // probe created; no sidecar => size unknown

		b := newBranch(src.Name)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: "default"}
		reconcileBranch(ctx, key, 2)

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: b.Spec.PVCName, Namespace: "default"},
			&corev1.PersistentVolumeClaim{}))).To(BeTrue(), "no PVC may exist before the size is known")
		var got volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchPending))

		// Size lands -> the branch proceeds with the real size.
		fillProbeRestoreSize(ctx, src.Name, 2<<30)
		reconcileSource(ctx, src.Name)
		reconcileBranch(ctx, key, 2)
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b.Spec.PVCName, Namespace: "default"}, &pvc)).To(Succeed())
		q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(q.Value()).To(Equal(int64(2 << 30)))
	})

	It("accepts a declared spec.sizeBytes when no originating content exists", func() {
		sc, vsc := uniqueName("sc"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, testDriver)
		src := newSource(uniqueName("src"), sc, vsc)
		src.Spec.SizeBytes = 4 << 30
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		reconcileSource(ctx, src.Name)

		var got volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: src.Name}, &got)).To(Succeed())
		Expect(got.Status.SizeBytes).To(Equal(int64(4 << 30)))
	})

	It("resolves an unset BranchPool.maxWarming from the source profile", func() {
		// zfs profile: MaxWarmingDefault=8, so a targetWarm=3 pool with no
		// explicit cap starts all three clones in one pass (budget 8 > need 3).
		srcName, _ := readySourceWithSC(ctx)
		bp := newPool(srcName, 3, nil)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 1)
		Expect(warmPVCs(ctx, srcName)).To(HaveLen(3),
			"profile MaxWarmingDefault=8 must not throttle 3 concurrent warmings")
	})

	It("sizes warm-pool PVCs from the source size in actual mode", func() {
		srcName, _ := readySourceWithSC(ctx)
		var src volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: srcName}, &src)).To(Succeed())
		src.Status.SizeBytes = 3 << 30
		Expect(k8sClient.Status().Update(ctx, &src)).To(Succeed())

		bp := newPool(srcName, 1, nil)
		Expect(k8sClient.Create(ctx, bp)).To(Succeed())
		reconcilePool(ctx, bp.Name, 1)

		warms := warmPVCs(ctx, srcName)
		Expect(warms).To(HaveLen(1))
		q := warms[0].Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(q.Value()).To(Equal(int64(3 << 30)))
	})

	It("tears down with the pins=false ordering without leaking any object", func() {
		// testDriver's builtin has SnapshotPinsVolume=false: snapshot objects
		// may be deleted before the volume. End state must be identical to
		// the pins=true path — everything gone.
		srcName := readySource(ctx)
		b := newBranch(srcName)
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		key := types.NamespacedName{Name: b.Name, Namespace: "default"}
		reconcileBranch(ctx, key, 2)

		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		reconcileBranch(ctx, key, 3)
		Expect(pool.FindClaimedPVC(ctx, k8sClient, srcName, string(b.UID))).To(BeNil())
	})
})
