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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/verify"
)

const condNS = "default"

// readyCond fetches the Ready condition off a fresh read of the object.
func sourceReadyCond(ctx context.Context, name string) *metav1.Condition {
	var got volumesv1alpha1.BranchSource
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got)).To(Succeed())
	return meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
}

func branchReadyCond(ctx context.Context, key types.NamespacedName) *metav1.Condition {
	var got volumesv1alpha1.Branch
	Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
	return meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
}

var _ = Describe("Ready conditions", func() {
	ctx := context.Background()

	It("sets Ready=True/Validated on a fully usable source", func() {
		name := readySource(ctx)
		cond := sourceReadyCond(ctx, name)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonValidated))
	})

	It("sets Ready=False/ClassMissing when the StorageClass is absent", func() {
		name := uniqueName("cond-nosc")
		Expect(k8sClient.Create(ctx, newSource(name, uniqueName("absent"), testVSC))).To(Succeed())
		reconcileSource(ctx, name)

		cond := sourceReadyCond(ctx, name)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonClassMissing))
		Expect(cond.Message).To(ContainSubstring("StorageClass"))
	})

	It("sets Ready=False/SizeUnknown while actual-mode sizing holds, then flips to Validated", func() {
		// testDriver's builtin profile is actual sizing: with no originating
		// content to discover a size from, the source validates (phase Ready)
		// but cannot produce clones yet.
		sc, vsc := uniqueName("sc"), uniqueName("vsc")
		createClasses(ctx, sc, vsc, testDriver)
		name := uniqueName("cond-nosize")
		Expect(k8sClient.Create(ctx, newSource(name, sc, vsc))).To(Succeed())
		reconcileSource(ctx, name)

		var got volumesv1alpha1.BranchSource
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(volumesv1alpha1.BranchSourceReady))
		cond := sourceReadyCond(ctx, name)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonSizeUnknown))

		fillProbeRestoreSize(ctx, name, 1<<30)
		reconcileSource(ctx, name)
		cond = sourceReadyCond(ctx, name)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonValidated))
	})

	It("walks a Branch from WaitingForSource to Provisioned", func() {
		name := uniqueName("cond-branch")
		key := types.NamespacedName{Name: name, Namespace: condNS}
		b := &volumesv1alpha1.Branch{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: condNS},
			Spec: volumesv1alpha1.BranchSpec{
				Source:  uniqueName("cond-absent-src"),
				PVCName: name + "-pvc",
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		reconcileBranch(ctx, key, 2)

		cond := branchReadyCond(ctx, key)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonWaitingForSource))

		// Point it at a real source; it should progress Cloning -> Provisioned.
		srcName := readySource(ctx)
		var cur volumesv1alpha1.Branch
		Expect(k8sClient.Get(ctx, key, &cur)).To(Succeed())
		cur.Spec.Source = srcName
		Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
		reconcileBranch(ctx, key, 2)

		cond = branchReadyCond(ctx, key)
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonCloning))

		bindPVC(ctx, name+"-pvc")
		reconcileBranch(ctx, key, 1)
		cond = branchReadyCond(ctx, key)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(volumesv1alpha1.ReasonProvisioned))
		Expect(cond.Message).To(ContainSubstring("ondemand"))
	})
})

var _ = Describe("verify preflight (against envtest)", func() {
	ctx := context.Background()

	It("passes the CRD checks and renders a report", func() {
		disc, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		var out strings.Builder
		_, err = verify.Run(ctx, k8sClient, disc, &out)
		Expect(err).NotTo(HaveOccurred())
		report := out.String()
		Expect(report).To(ContainSubstring("ok    volumes.arbit-tech.com/v1alpha1/branchsources"))
		Expect(report).To(ContainSubstring("ok    snapshot.storage.k8s.io/v1/volumesnapshots"))
		// envtest runs no snapshot-controller: that is a warning, not a failure.
		Expect(report).To(ContainSubstring("WARN  no deployment named *snapshot-controller*"))
	})
})
