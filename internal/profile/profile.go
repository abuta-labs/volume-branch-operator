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

// Package profile encodes per-backend storage policy — the substrate
// profile. CSI drivers differ in ways the CSI spec does not surface: whether
// a snapshot pins its parent volume, whether clone size requests are honored
// or must be exact, how many concurrent CreateVolume calls the backend
// tolerates. The engine ships built-in profiles for known drivers, falls
// back to conservative values for unknown ones, and lets a BranchSource
// override any field via spec.profile.
package profile

import (
	"k8s.io/apimachinery/pkg/api/resource"

	volumesv1alpha1 "github.com/abuta-labs/volume-branch-operator/api/v1alpha1"
)

// Profile is the resolved, effective policy for one BranchSource.
type Profile struct {
	// SnapshotPinsVolume: on this backend a snapshot pins its parent volume,
	// so any teardown that performs PHYSICAL deletions must remove volumes
	// before the snapshots they came from. (Per-branch snapshot objects in
	// this engine are Retain-policy — Kubernetes-object-only deletes — so
	// the flag governs orderings only where the engine actually destroys
	// backend state.)
	SnapshotPinsVolume bool

	// CloneSizeMode: sentinel (fixed placeholder request) or actual (request
	// the source's real size, for drivers that enforce request >= restore
	// size).
	CloneSizeMode volumesv1alpha1.CloneSizeMode

	// CloneSizeSentinel is the sentinel-mode request, and the actual-mode
	// fallback while the source's size is still unknown.
	CloneSizeSentinel resource.Quantity

	// MaxWarmingDefault caps concurrent warm-clone creations when
	// BranchPool.spec.maxWarming is unset. Backends that serialize
	// CreateVolume (some to ~1/min) need a low cap — flooding them just
	// queues failures; fast-clone backends can go wide.
	MaxWarmingDefault int32
}

// sizeGranularity is what actual-mode requests round up to. 1Mi keeps
// requests honest to the byte range while avoiding odd-byte quantities that
// render unreadably in kubectl.
const sizeGranularity = 1 << 20

// defaultSentinel is the built-in sentinel request shared by all profiles.
func defaultSentinel() resource.Quantity { return resource.MustParse("1Gi") }

// Builtin returns the built-in profile for a CSI driver name. Unknown
// drivers get the conservative profile: pins=true (the stricter teardown
// ordering is safe everywhere), sentinel sizing (never over-asks quota), and
// a low warming cap (never floods a serializing backend).
func Builtin(driver string) Profile {
	switch driver {
	case "zfs.csi.openebs.io":
		// Local ZFS: clones are instant and cheap to create concurrently;
		// the driver enforces request >= the snapshot's restore size, so
		// sizing must be actual.
		return Profile{
			SnapshotPinsVolume: false,
			CloneSizeMode:      volumesv1alpha1.CloneSizeModeActual,
			CloneSizeSentinel:  defaultSentinel(),
			MaxWarmingDefault:  8,
		}
	case "fsx.openzfs.csi.aws.com":
		// FSx for OpenZFS: snapshots pin their parent volume; clones are
		// always full-size views (the request is a placeholder the driver
		// ignores); CreateVolume is serialized backend-side, so warming
		// concurrency must stay low.
		return Profile{
			SnapshotPinsVolume: true,
			CloneSizeMode:      volumesv1alpha1.CloneSizeModeSentinel,
			CloneSizeSentinel:  defaultSentinel(),
			MaxWarmingDefault:  2,
		}
	default:
		return Profile{
			SnapshotPinsVolume: true,
			CloneSizeMode:      volumesv1alpha1.CloneSizeModeSentinel,
			CloneSizeSentinel:  defaultSentinel(),
			MaxWarmingDefault:  2,
		}
	}
}

// Resolve returns the effective profile for a source: the built-in profile
// for its CSI driver with spec.profile overrides applied field-by-field.
func Resolve(src *volumesv1alpha1.BranchSource) Profile {
	p := Builtin(src.Spec.CSIDriver)
	o := src.Spec.Profile
	if o == nil {
		return p
	}
	if o.SnapshotPinsVolume != nil {
		p.SnapshotPinsVolume = *o.SnapshotPinsVolume
	}
	if o.CloneSizeMode != nil {
		p.CloneSizeMode = *o.CloneSizeMode
	}
	if o.CloneSizeSentinel != nil {
		p.CloneSizeSentinel = *o.CloneSizeSentinel
	}
	if o.MaxWarmingDefault != nil {
		p.MaxWarmingDefault = *o.MaxWarmingDefault
	}
	return p
}

// NeedsSize reports whether clone creation must wait for the source's size:
// actual mode with an unknown size may NOT guess. A sentinel-sized request
// below the snapshot's restore size is refused by size-enforcing drivers and
// the mis-sized PVC then wedges permanently (PVC requests cannot shrink or
// be re-negotiated) — observed live on zfs-localpv.
func (p Profile) NeedsSize() bool {
	return p.CloneSizeMode == volumesv1alpha1.CloneSizeModeActual
}

// SizeRequest computes the capacity request for a clone PVC of a source
// whose size is sizeBytes. Actual mode requests the size rounded up to 1Mi;
// sentinel mode requests the fixed placeholder. Callers in actual mode must
// hold off until sizeBytes is known (NeedsSize) — the sentinel return for
// sizeBytes=0 is a defensive last resort, not a supported path.
func (p Profile) SizeRequest(sizeBytes int64) resource.Quantity {
	if p.CloneSizeMode == volumesv1alpha1.CloneSizeModeActual && sizeBytes > 0 {
		rounded := (sizeBytes + sizeGranularity - 1) / sizeGranularity * sizeGranularity
		return *resource.NewQuantity(rounded, resource.BinarySI)
	}
	return p.CloneSizeSentinel
}

// Resolved renders the profile into the API shape published on
// BranchSource.status.
func (p Profile) Resolved() *volumesv1alpha1.ResolvedProfile {
	return &volumesv1alpha1.ResolvedProfile{
		SnapshotPinsVolume: p.SnapshotPinsVolume,
		CloneSizeMode:      p.CloneSizeMode,
		CloneSizeSentinel:  p.CloneSizeSentinel.String(),
		MaxWarmingDefault:  p.MaxWarmingDefault,
	}
}
