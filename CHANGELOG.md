# Changelog

## v0.1.0-alpha.3

**BREAKING — reinstall required.** The project moved to the renamed GitHub
account `abuta-labs`, and with it everything derived from the old account
name:

- **API group renamed** to `volumes.abuta-labs.com/v1alpha1` (CRD kinds,
  finalizers, and label keys all move with it). There is **no conversion**
  from the previous group: uninstall any v0.1.0-alpha.2 deployment (delete
  its Branches/BranchPools/BranchSources first so finalizer teardown runs,
  then remove the old CRDs) and install fresh.
- Go module path is now `github.com/abuta-labs/volume-branch-operator`.
- Operator images publish to `ghcr.io/abuta-labs/volume-branch-operator`.

No functional changes.

## v0.1.0-alpha.1

First tagged release. Alpha: the `volumes.arbit-tech.com/v1alpha1` API — as
this release shipped it, before the v0.1.0-alpha.3 rename — may change
without notice.

### Engine

- **CRDs**: `BranchSource` (cluster-scoped immutable snapshot handle +
  class pair), `BranchPool` (cluster-scoped warm set), `Branch` (namespaced;
  terminates at a Bound PVC the consumer names).
- **On-demand branching**: per-branch VolumeSnapshotContent (Retain,
  statically bound) + VolumeSnapshot + PVC from the source's snapshot
  handle; finalizer teardown ordered for snapshot-pins-volume backends, with
  a leak guard on the cluster-scoped content.
- **Pool fast path**: pre-warmed clones claimed by a race-safe two-phase
  claim — non-destructive optimistic label flip, durably recorded on the
  Branch before the rebind consumes the warm clone; PV rebind pre-binds the
  ClaimRef so there is no theft window. Claim-to-Ready measured at ~6.5 s vs
  ~17 s on-demand on ZFS-LocalPV. Stale-warming and orphan-claim reapers.
- **Reset and TTL**: `spec.resetToken` re-branches from the source
  (any-direction change, including to/from empty); optional `spec.ttl`
  self-cleanup after Ready.
- **Substrate profiles**: per-driver policy (teardown ordering, clone sizing
  mode, warming concurrency) resolved per source — builtins for
  ZFS-LocalPV and FSx for OpenZFS, conservative defaults for unknown
  drivers, per-field `spec.profile` overrides, effective values published on
  `status.resolvedProfile`. Actual-mode sizing discovers the source size
  from the snapshot's originating VolumeSnapshotContent (or `spec.sizeBytes`)
  and holds clone creation rather than guessing.
- **Conditions + verify**: standard `Ready` conditions on BranchSource and
  Branch; `manager verify` read-only cluster preflight (CRDs,
  snapshot-controller, per-source class/driver coherence, resolved
  profiles).

### Validation

- Full e2e suite on **OpenEBS ZFS-LocalPV** in kind (seed data carried into
  branches, CoW write isolation, pool claim, reset, TTL reap, leak-free
  teardown) — runs on every PR in CI and is required to merge, alongside
  envtest and lint.
- **FSx for OpenZFS: profile defined, e2e pending.** No verified FSx run
  yet; treat FSx support as unproven in this release.
