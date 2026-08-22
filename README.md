# volume-branch-operator

[![ci](https://github.com/arbit-tech/volume-branch-operator/actions/workflows/ci.yaml/badge.svg)](https://github.com/arbit-tech/volume-branch-operator/actions/workflows/ci.yaml)

A Kubernetes operator for **volume branching**: instant copy-on-write clones
of stateful volumes, kept warm in pools so a branch is ready in seconds
instead of your storage backend's CreateVolume latency — over snapshot-capable
CSI drivers.

## The problem

CSI gives Kubernetes the primitives — `VolumeSnapshot`, snapshot-sourced
PVCs — but nothing orchestrates *branch lifecycle* on top of them:

- a named, immutable **source** to branch from
- N **pre-warmed clones** waiting in a pool, so claiming a branch is a label
  flip, not a slow CreateVolume call
- atomic **claim** into a target namespace, **reset** (re-branch from the
  same source), optional **TTL**, and ordered **reclaim** that respects
  backend quirks (snapshot-pins-volume rules, clone-rate limits, sizing
  modes)

Database-level cloning tools (CNPG, Zalando) solve this per-engine. This
operator solves it once, at the volume layer, for anything that runs on a
PVC. It is **not** a storage backend selector — `storageClassName` already
does that — and not a CSI driver: it orchestrates standard snapshot objects
over the drivers you already run.

## Status

**Alpha. The API (`volumes.arbit-tech.com/v1alpha1`) may change without
notice.**

| Driver | Status |
|---|---|
| OpenEBS ZFS-LocalPV (`zfs.csi.openebs.io`) | **Proven** — the full e2e suite runs against it in CI |
| Amazon FSx for OpenZFS (`fsx.openzfs.csi.aws.com`) | **Proven** — full lifecycle e2e passed 2026-08-22 on EKS + FSx (sentinel sizing, warming cap, pool claim 11.8s vs ~100s on-demand); see [docs/fsx-gate.md](docs/fsx-gate.md) |
| Other snapshot-capable drivers | Conservative default profile; untested |

## How it works

Three CRDs:

| Kind | Scope | Purpose |
|---|---|---|
| `BranchSource` | Cluster | An immutable CSI snapshot handle to branch from, plus the StorageClass / VolumeSnapshotClass pair to branch with |
| `BranchPool` | Cluster | A warm set of pre-cloned volumes against one source |
| `Branch` | Namespaced | One claimed clone, terminating at a Bound PVC **you name** in the Branch's namespace |

A `Branch` is satisfied from a pool when one has stock (a metadata-only
claim: label flip → PV rebind into your namespace; measured at ~6.5 s vs
~17 s on-demand even on instant-clone ZFS) and provisioned on demand
otherwise (per-branch VolumeSnapshotContent + VolumeSnapshot + PVC from the
source's handle). The engine stops at the Bound PVC: what runs on the volume
is your business.

```yaml
apiVersion: volumes.arbit-tech.com/v1alpha1
kind: Branch
metadata:
  name: alice-dev
  namespace: default
spec:
  source: golden-v1        # a BranchSource
  pvcName: alice-dev-data  # the PVC the engine must produce here
  ttl: 4h                  # optional: self-cleanup after Ready
```

Complete walk-throughs: [`config/samples/zfs-localpv/`](config/samples/zfs-localpv/)
(proven) and [`config/samples/fsx-openzfs/`](config/samples/fsx-openzfs/)
(sketch, e2e pending).

## Quickstart (kind + ZFS-LocalPV)

Requirements: docker, kind, kubectl, and the `zfs` kernel module loaded on
the host (`sudo modprobe zfs`; the userland tools and the zpool live inside
the kind node — no host root needed).

```sh
# Cluster + snapshotter + ZFS-LocalPV + a zpool + classes:
hack/e2e-up.sh

# Build and deploy the operator into it:
make docker-build IMG=vbo:dev
kind load docker-image vbo:dev --name volume-branch-operator-test-e2e
make deploy IMG=vbo:dev

# Then follow config/samples/zfs-localpv/README.md:
# seed PVC -> snapshot -> BranchSource -> BranchPool -> Branch

hack/e2e-down.sh   # tears everything down
```

Tagged releases ship a single install manifest (CRDs + operator) as a
release asset:

```sh
kubectl apply -f https://github.com/arbit-tech/volume-branch-operator/releases/latest/download/install.yaml
```

## CRD reference

### BranchSource (cluster-scoped)

| Field | Description |
|---|---|
| `spec.snapshotHandle` | CSI snapshot handle (driver-specific id) of the immutable source snapshot |
| `spec.csiDriver` | CSI driver that owns the handle; both classes must belong to it |
| `spec.cloneStorageClassName` | StorageClass for clone PVCs |
| `spec.volumeSnapshotClassName` | VolumeSnapshotClass for per-branch snapshot objects |
| `spec.profile` | Optional per-field overrides of the driver's built-in profile |
| `spec.sizeBytes` | Optional declared snapshot size (see sizing below) |
| `status.phase` | `Pending` / `Ready` / `Invalid` (validation state) |
| `status.conditions` | `Ready` condition — True only when clones can be created *now* (stricter than phase: a validated source still discovering its size is `Ready=False/SizeUnknown`) |
| `status.sizeBytes` | Discovered or declared snapshot size |
| `status.resolvedProfile` | The effective profile (builtin ← overrides) |

### BranchPool (cluster-scoped)

| Field | Description |
|---|---|
| `spec.source` | The BranchSource to keep warm clones of |
| `spec.targetWarm` | How many ready-to-claim clones to keep (0 is valid) |
| `spec.maxWarming` | Cap on concurrent clone creation; unset = the source profile's default |
| `status.warm` / `status.warming` / `status.claimedTotal` | Pool inventory and a lifetime claim counter |

Warm clones live in a single holding namespace (`branch-pool` by default,
`--pool-namespace` on the manager).

### Branch (namespaced)

| Field | Description |
|---|---|
| `spec.source` | The BranchSource to branch from |
| `spec.pvcName` | The PVC the engine must produce in this namespace — the consumer names it, the engine never invents names |
| `spec.resetToken` | Change it (any direction, including to/from empty) to discard the clone and re-branch |
| `spec.ttl` | Optional: delete the Branch this long after it becomes Ready |
| `status.phase` | `Pending` / `Cloning` / `Ready` / `Failed` |
| `status.conditions` | `Ready` condition (reasons: `WaitingForSource`, `WaitingForSize`, `Cloning`, `Provisioned`) |
| `status.provisioning` | `pool` or `ondemand` — which path produced the clone (drives teardown ordering) |

## Substrate profiles

CSI drivers differ in ways the CSI spec does not surface. The engine encodes
those differences as a per-driver **profile**, resolved per BranchSource:
built-in values for known drivers, overridable field-by-field via
`spec.profile`, published on `status.resolvedProfile`.

| | `zfs.csi.openebs.io` | `fsx.openzfs.csi.aws.com` | unknown drivers |
|---|---|---|---|
| `snapshotPinsVolume` (delete volumes before snapshots) | false | true | true (safe everywhere) |
| `cloneSizeMode` | `actual` | `sentinel` | `sentinel` |
| `maxWarmingDefault` | 8 | 2 (FSx serializes CreateVolume ~1/min) | 2 |

**Sizing.** `sentinel` requests a fixed 1Gi placeholder — for drivers whose
clones are full-size views of their parent and ignore the request. `actual`
requests the source's real size — required on drivers that enforce
request ≥ the snapshot's restore size. In actual mode the engine will **not
guess**: a too-small request would wedge the PVC permanently (PVC requests
cannot shrink), so clone creation holds — `Branch` waits at
`Ready=False/WaitingForSize`, the source shows `SizeUnknown` — until the size
is known. It is discovered from the snapshot's originating
VolumeSnapshotContent (keep it around!), or declared explicitly in
`spec.sizeBytes`.

```yaml
spec:
  profile:            # per-source overrides, all fields optional
    maxWarmingDefault: 4
    cloneSizeMode: actual
```

## Verify

`manager verify` is a read-only preflight you can run against any cluster —
before installing sources, or while debugging one:

```sh
bin/manager verify --kubeconfig ~/.kube/config
```

```
CRDs
  ok    volumes.arbit-tech.com/v1alpha1/branchsources
  ok    volumes.arbit-tech.com/v1alpha1/branchpools
  ok    volumes.arbit-tech.com/v1alpha1/branches
  ok    snapshot.storage.k8s.io/v1/volumesnapshots
  ok    snapshot.storage.k8s.io/v1/volumesnapshotclasses
  ok    snapshot.storage.k8s.io/v1/volumesnapshotcontents
snapshot-controller
  ok    deployment kube-system/snapshot-controller (1 ready)
BranchSource golden-v1 (driver zfs.csi.openebs.io)
  ok    StorageClass "branch-clone" (provisioner zfs.csi.openebs.io)
  ok    VolumeSnapshotClass "branch-snap" (driver zfs.csi.openebs.io)
        profile: snapshotPinsVolume=false cloneSizeMode=actual sentinel=1Gi maxWarmingDefault=8
verify: 0 failure(s), 0 warning(s)
```

It exits non-zero on failures; warnings (e.g. no deployment named
`*snapshot-controller*` — some distributions run it under another name) do
not fail the run.

## Development

```sh
make test        # unit + envtest (needs no cluster)
make lint        # golangci-lint
make test-e2e    # full e2e: kind + ZFS-LocalPV (needs docker + zfs module)
make manifests generate   # after changing api/
```

The e2e harness (`hack/e2e-up.sh` / `hack/e2e-down.sh`) creates a kind
cluster with a loop-device-backed zpool *inside* the node container — the
host only needs the `zfs` kernel module loaded. CI runs lint, envtest, and
the full e2e on every PR; all three are required to merge.

Contributions welcome — especially verified runs and profiles for additional
CSI drivers. Open an issue first for anything behavior-changing.

## Provenance

The design is proven in production by the operator behind
[Adjoint](https://adjoint.sh), which runs branchable Postgres environments on
AWS FSx for OpenZFS; this repo is a standalone, generalized build of that
engine.

## License

Apache-2.0
