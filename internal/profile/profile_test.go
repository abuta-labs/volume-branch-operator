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

package profile

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
)

const gib = "1Gi"

func srcWith(driver string, o *volumesv1alpha1.ProfileOverrides) *volumesv1alpha1.BranchSource {
	return &volumesv1alpha1.BranchSource{
		ObjectMeta: metav1.ObjectMeta{Name: "s"},
		Spec: volumesv1alpha1.BranchSourceSpec{
			CSIDriver: driver,
			Profile:   o,
		},
	}
}

func TestBuiltinZFSLocalPV(t *testing.T) {
	p := Builtin("zfs.csi.openebs.io")
	if p.SnapshotPinsVolume {
		t.Error("zfs-localpv: SnapshotPinsVolume should be false")
	}
	if p.CloneSizeMode != volumesv1alpha1.CloneSizeModeActual {
		t.Errorf("zfs-localpv: CloneSizeMode = %q, want actual", p.CloneSizeMode)
	}
	if p.MaxWarmingDefault != 8 {
		t.Errorf("zfs-localpv: MaxWarmingDefault = %d, want 8", p.MaxWarmingDefault)
	}
}

func TestBuiltinFSxOpenZFS(t *testing.T) {
	p := Builtin("fsx.openzfs.csi.aws.com")
	if !p.SnapshotPinsVolume {
		t.Error("fsx: SnapshotPinsVolume should be true")
	}
	if p.CloneSizeMode != volumesv1alpha1.CloneSizeModeSentinel {
		t.Errorf("fsx: CloneSizeMode = %q, want sentinel", p.CloneSizeMode)
	}
	if p.MaxWarmingDefault != 2 {
		t.Errorf("fsx: MaxWarmingDefault = %d, want 2", p.MaxWarmingDefault)
	}
}

func TestBuiltinUnknownDriverIsConservative(t *testing.T) {
	p := Builtin("does.not.exist.example.com")
	if !p.SnapshotPinsVolume {
		t.Error("unknown driver: SnapshotPinsVolume should default true (safe everywhere)")
	}
	if p.CloneSizeMode != volumesv1alpha1.CloneSizeModeSentinel {
		t.Errorf("unknown driver: CloneSizeMode = %q, want sentinel", p.CloneSizeMode)
	}
	if p.MaxWarmingDefault != 2 {
		t.Errorf("unknown driver: MaxWarmingDefault = %d, want 2", p.MaxWarmingDefault)
	}
	if got := p.CloneSizeSentinel.String(); got != gib {
		t.Errorf("unknown driver: sentinel = %s, want 1Gi", got)
	}
}

func TestResolveOverridesFieldByField(t *testing.T) {
	q := resource.MustParse("2Gi")
	mode := volumesv1alpha1.CloneSizeModeActual
	p := Resolve(srcWith("fsx.openzfs.csi.aws.com", &volumesv1alpha1.ProfileOverrides{
		CloneSizeMode:     &mode,
		CloneSizeSentinel: &q,
		MaxWarmingDefault: ptr.To(int32(5)),
	}))
	if p.CloneSizeMode != volumesv1alpha1.CloneSizeModeActual {
		t.Errorf("override lost: CloneSizeMode = %q", p.CloneSizeMode)
	}
	if p.CloneSizeSentinel.String() != "2Gi" {
		t.Errorf("override lost: sentinel = %s", p.CloneSizeSentinel.String())
	}
	if p.MaxWarmingDefault != 5 {
		t.Errorf("override lost: MaxWarmingDefault = %d", p.MaxWarmingDefault)
	}
	// The untouched field keeps its builtin value.
	if !p.SnapshotPinsVolume {
		t.Error("unset override must keep the builtin SnapshotPinsVolume=true")
	}
}

func TestResolveNilOverridesIsBuiltin(t *testing.T) {
	if got, want := Resolve(srcWith("zfs.csi.openebs.io", nil)), Builtin("zfs.csi.openebs.io"); got != want {
		t.Errorf("Resolve(nil overrides) = %+v, want builtin %+v", got, want)
	}
}

func TestSizeRequest(t *testing.T) {
	actual := Profile{CloneSizeMode: volumesv1alpha1.CloneSizeModeActual, CloneSizeSentinel: resource.MustParse(gib)}
	sentinel := Profile{CloneSizeMode: volumesv1alpha1.CloneSizeModeSentinel, CloneSizeSentinel: resource.MustParse(gib)}

	// Actual mode rounds up to 1Mi.
	if got := actual.SizeRequest(1<<30 + 1); got.Value() != 1<<30+1<<20 {
		t.Errorf("actual: got %d bytes, want 1Gi+1Mi", got.Value())
	}
	// An exact multiple stays exact.
	if got := actual.SizeRequest(2 << 30); got.Value() != 2<<30 {
		t.Errorf("actual: got %d bytes, want exactly 2Gi", got.Value())
	}
	// Unknown size in actual mode falls back to the sentinel.
	if got := actual.SizeRequest(0); got.String() != gib {
		t.Errorf("actual with unknown size: got %s, want sentinel 1Gi", got.String())
	}
	// Sentinel mode ignores the size entirely.
	if got := sentinel.SizeRequest(64 << 30); got.String() != gib {
		t.Errorf("sentinel: got %s, want 1Gi", got.String())
	}
}

func TestResolvedRendering(t *testing.T) {
	r := Builtin("zfs.csi.openebs.io").Resolved()
	if r.CloneSizeMode != volumesv1alpha1.CloneSizeModeActual || r.MaxWarmingDefault != 8 ||
		r.SnapshotPinsVolume || r.CloneSizeSentinel != gib {
		t.Errorf("Resolved() = %+v", r)
	}
}
