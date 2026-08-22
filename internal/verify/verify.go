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

// Package verify is the read-only cluster preflight behind
// `manager verify`: is this cluster able to run the engine, and are its
// BranchSources coherent? It mutates nothing — every check is a read — so it
// is safe to run against production at any time.
package verify

import (
	"context"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"

	volumesv1alpha1 "github.com/arbit-tech/volume-branch-operator/api/v1alpha1"
	"github.com/arbit-tech/volume-branch-operator/internal/profile"
)

// Outcome aggregates the run: OK means every check passed (warnings do not
// fail a run — they flag things the engine can work without but an operator
// should know about).
type Outcome struct {
	Failures int
	Warnings int
}

// OK reports whether the preflight passed.
func (o Outcome) OK() bool { return o.Failures == 0 }

// Run executes the preflight and renders a human-readable report to w.
func Run(ctx context.Context, c client.Client, disc discovery.DiscoveryInterface, w io.Writer) (Outcome, error) {
	var o Outcome
	// Report writes go to a human; a failed write has nowhere better to go.
	out := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	pass := func(format string, a ...any) { out("  ok    "+format, a...) }
	warn := func(format string, a ...any) { o.Warnings++; out("  WARN  "+format, a...) }
	fail := func(format string, a ...any) { o.Failures++; out("  FAIL  "+format, a...) }

	out("%s", "CRDs")
	if err := checkGroup(disc, volumesv1alpha1.GroupVersion.String(),
		[]string{"branchsources", "branchpools", "branches"}, pass, fail); err != nil {
		return o, err
	}
	// The engine drives everything through snapshot objects; without these
	// CRDs (and the snapshot-controller reconciling them) nothing binds.
	if err := checkGroup(disc, "snapshot.storage.k8s.io/v1",
		[]string{"volumesnapshots", "volumesnapshotclasses", "volumesnapshotcontents"}, pass, fail); err != nil {
		return o, err
	}

	out("%s", "snapshot-controller")
	// A deployment named *snapshot-controller* is the stock install, but some
	// distributions embed it elsewhere — its absence is a warning, not proof
	// of a broken cluster.
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps); err != nil {
		return o, err
	}
	found := false
	for i := range deps.Items {
		if strings.Contains(deps.Items[i].Name, "snapshot-controller") {
			found = true
			d := &deps.Items[i]
			if d.Status.ReadyReplicas > 0 {
				pass("deployment %s/%s (%d ready)", d.Namespace, d.Name, d.Status.ReadyReplicas)
			} else {
				fail("deployment %s/%s has no ready replicas", d.Namespace, d.Name)
			}
		}
	}
	if !found {
		warn("no deployment named *snapshot-controller* found — fine if your distribution runs it under another name, fatal otherwise")
	}

	var sources volumesv1alpha1.BranchSourceList
	if err := c.List(ctx, &sources); err != nil {
		// Absent CRDs were already reported above; don't double-fail.
		if !apierrors.IsNotFound(err) {
			warn("cannot list BranchSources: %v", err)
		}
		return o, nil
	}
	if len(sources.Items) == 0 {
		out("%s", "BranchSources: none defined")
		return o, nil
	}
	for i := range sources.Items {
		src := &sources.Items[i]
		fmt.Fprintf(w, "BranchSource %s (driver %s)\n", src.Name, src.Spec.CSIDriver)
		verifySource(ctx, c, src, pass, fail)
		p := profile.Resolve(src)
		fmt.Fprintf(w, "        profile: snapshotPinsVolume=%t cloneSizeMode=%s sentinel=%s maxWarmingDefault=%d\n",
			p.SnapshotPinsVolume, p.CloneSizeMode, p.CloneSizeSentinel.String(), p.MaxWarmingDefault)
		if p.NeedsSize() && src.Status.SizeBytes == 0 && src.Spec.SizeBytes == 0 {
			warn("actual-mode sizing with no known size yet — clone creation holds until discovery finds the originating VolumeSnapshotContent, or spec.sizeBytes is declared")
		}
	}
	return o, nil
}

// checkGroup fails per missing resource of one API group/version.
func checkGroup(disc discovery.DiscoveryInterface, gv string, want []string, pass, fail func(string, ...any)) error {
	list, err := disc.ServerResourcesForGroupVersion(gv)
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) || apierrors.IsNotFound(err) {
			for _, res := range want {
				fail("%s: %s not installed", gv, res)
			}
			return nil
		}
		return err
	}
	have := map[string]bool{}
	for _, r := range list.APIResources {
		have[r.Name] = true
	}
	for _, res := range want {
		if have[res] {
			pass("%s/%s", gv, res)
		} else {
			fail("%s: %s not installed", gv, res)
		}
	}
	return nil
}

// verifySource re-runs the class checks the controller performs, so the
// preflight is useful even while the operator is not running.
func verifySource(ctx context.Context, c client.Client, src *volumesv1alpha1.BranchSource, pass, fail func(string, ...any)) {
	var sc storagev1.StorageClass
	switch err := c.Get(ctx, client.ObjectKey{Name: src.Spec.CloneStorageClassName}, &sc); {
	case apierrors.IsNotFound(err):
		fail("StorageClass %q not found", src.Spec.CloneStorageClassName)
	case err != nil:
		fail("StorageClass %q: %v", src.Spec.CloneStorageClassName, err)
	case sc.Provisioner != src.Spec.CSIDriver:
		fail("StorageClass %q is provisioned by %q, not %q", sc.Name, sc.Provisioner, src.Spec.CSIDriver)
	default:
		pass("StorageClass %q (provisioner %s)", sc.Name, sc.Provisioner)
	}

	var vsc snapv1.VolumeSnapshotClass
	switch err := c.Get(ctx, client.ObjectKey{Name: src.Spec.VolumeSnapshotClassName}, &vsc); {
	case apierrors.IsNotFound(err):
		fail("VolumeSnapshotClass %q not found", src.Spec.VolumeSnapshotClassName)
	case err != nil:
		fail("VolumeSnapshotClass %q: %v", src.Spec.VolumeSnapshotClassName, err)
	case vsc.Driver != src.Spec.CSIDriver:
		fail("VolumeSnapshotClass %q belongs to driver %q, not %q", vsc.Name, vsc.Driver, src.Spec.CSIDriver)
	default:
		pass("VolumeSnapshotClass %q (driver %s)", vsc.Name, vsc.Driver)
	}
}
