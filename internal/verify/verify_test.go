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

package verify

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
)

const zfsDriver = "zfs.csi.openebs.io"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, volumesv1alpha1.AddToScheme, snapv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// fakeDisc serves the given group/versions, each with the resource names
// mapped to it.
func fakeDisc(groups map[string][]string) *discoveryfake.FakeDiscovery {
	lists := make([]*metav1.APIResourceList, 0, len(groups))
	for gv, names := range groups {
		l := &metav1.APIResourceList{GroupVersion: gv}
		for _, n := range names {
			l.APIResources = append(l.APIResources, metav1.APIResource{Name: n})
		}
		lists = append(lists, l)
	}
	return &discoveryfake.FakeDiscovery{Fake: &clientgotesting.Fake{Resources: lists}}
}

func fullDisc() *discoveryfake.FakeDiscovery {
	return fakeDisc(map[string][]string{
		"volumes.arbit-tech.com/v1alpha1": {"branchsources", "branchpools", "branches"},
		"snapshot.storage.k8s.io/v1":      {"volumesnapshots", "volumesnapshotclasses", "volumesnapshotcontents"},
	})
}

func snapController() *appsv1.Deployment {
	const ready = 1
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-controller", Namespace: "kube-system"},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func validSource(name string) []client.Object {
	return []client.Object{
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: name + "-sc"},
			Provisioner: zfsDriver,
		},
		&snapv1.VolumeSnapshotClass{
			ObjectMeta:     metav1.ObjectMeta{Name: name + "-vsc"},
			Driver:         zfsDriver,
			DeletionPolicy: snapv1.VolumeSnapshotContentRetain,
		},
		&volumesv1alpha1.BranchSource{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: volumesv1alpha1.BranchSourceSpec{
				SnapshotHandle:          "h-" + name,
				CSIDriver:               zfsDriver,
				CloneStorageClassName:   name + "-sc",
				VolumeSnapshotClassName: name + "-vsc",
				SizeBytes:               1 << 30,
			},
		},
	}
}

func run(t *testing.T, disc *discoveryfake.FakeDiscovery, objs ...client.Object) (Outcome, string) {
	t.Helper()
	c := clientfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	var out strings.Builder
	o, err := Run(context.Background(), c, disc, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return o, out.String()
}

func TestVerifyHappyPath(t *testing.T) {
	objs := append(validSource("src"), snapController())
	o, report := run(t, fullDisc(), objs...)
	if o.Failures != 0 {
		t.Fatalf("expected 0 failures, got %d:\n%s", o.Failures, report)
	}
	if o.Warnings != 0 {
		t.Fatalf("expected 0 warnings, got %d:\n%s", o.Warnings, report)
	}
	if !strings.Contains(report, "profile: snapshotPinsVolume=false cloneSizeMode=actual") {
		t.Fatalf("resolved profile not printed:\n%s", report)
	}
}

func TestVerifyMissingSnapshotCRDs(t *testing.T) {
	disc := fakeDisc(map[string][]string{
		"volumes.arbit-tech.com/v1alpha1": {"branchsources", "branchpools", "branches"},
	})
	o, report := run(t, disc, snapController())
	if o.Failures != 3 {
		t.Fatalf("expected 3 failures (one per snapshot CRD), got %d:\n%s", o.Failures, report)
	}
	if o.OK() {
		t.Fatal("outcome should not be OK")
	}
}

func TestVerifyNoSnapshotController(t *testing.T) {
	o, report := run(t, fullDisc(), validSource("src")...)
	if o.Failures != 0 {
		t.Fatalf("absent snapshot-controller must warn, not fail:\n%s", report)
	}
	if o.Warnings != 1 || !strings.Contains(report, "WARN") {
		t.Fatalf("expected exactly one warning:\n%s", report)
	}
}

func TestVerifySourceProblems(t *testing.T) {
	// Missing StorageClass + mismatched snapshot-class driver: two failures.
	objs := []client.Object{
		snapController(),
		&snapv1.VolumeSnapshotClass{
			ObjectMeta:     metav1.ObjectMeta{Name: "other-vsc"},
			Driver:         "other.example.com",
			DeletionPolicy: snapv1.VolumeSnapshotContentRetain,
		},
		&volumesv1alpha1.BranchSource{
			ObjectMeta: metav1.ObjectMeta{Name: "bad"},
			Spec: volumesv1alpha1.BranchSourceSpec{
				SnapshotHandle:          "h-bad",
				CSIDriver:               zfsDriver,
				CloneStorageClassName:   "absent-sc",
				VolumeSnapshotClassName: "other-vsc",
				SizeBytes:               1 << 30,
			},
		},
	}
	o, report := run(t, fullDisc(), objs...)
	if o.Failures != 2 {
		t.Fatalf("expected 2 failures, got %d:\n%s", o.Failures, report)
	}
	if !strings.Contains(report, "not found") || !strings.Contains(report, "belongs to driver") {
		t.Fatalf("failure detail missing:\n%s", report)
	}
}

func TestVerifyActualModeSizeWarning(t *testing.T) {
	objs := append(validSource("src"), snapController())
	// Strip the declared size: actual mode with no size anywhere must warn.
	for _, obj := range objs {
		if src, ok := obj.(*volumesv1alpha1.BranchSource); ok {
			src.Spec.SizeBytes = 0
		}
	}
	o, report := run(t, fullDisc(), objs...)
	if o.Failures != 0 || o.Warnings != 1 {
		t.Fatalf("expected only the size warning, got %d failures %d warnings:\n%s",
			o.Failures, o.Warnings, report)
	}
	if !strings.Contains(report, "actual-mode sizing") {
		t.Fatalf("size warning missing:\n%s", report)
	}
}
