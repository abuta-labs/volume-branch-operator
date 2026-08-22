# The FSx gate — running the e2e suite against Amazon FSx for OpenZFS

The e2e suite is driver-parameterized: the same lifecycle specs that run
against ZFS-LocalPV in CI can run against any snapshot-capable CSI driver on
an existing cluster. The FSx gate is that run on `fsx.openzfs.csi.aws.com` —
the pass that backs the "proven" claim in the README status table.

## What you need

Any Kubernetes cluster with:

- the [FSx for OpenZFS CSI driver](https://github.com/kubernetes-sigs/aws-fsx-openzfs-csi-driver)
  and the external snapshot-controller installed,
- a StorageClass provisioning volumes as children of an FSx OpenZFS
  filesystem's root volume (`ResourceType: volume`, `ParentVolumeId: ...`),
- a **clone** StorageClass with `OriginSnapshot: {"CopyStrategy":"CLONE"}` —
  the copy-on-write switch; `FULL_COPY` would defeat the point,
- a VolumeSnapshotClass for the driver,
- the operator installed (`kubectl apply -f install.yaml` from a release).

How the cluster comes to exist is irrelevant to the gate — EKS with the
driver's Helm chart is typical. FSx OpenZFS volume PVCs must request exactly
1Gi (the driver mandates the sentinel; real capacity comes from the parent
filesystem) — which is exactly why the FSx profile resolves to sentinel
sizing.

## Running it

```sh
export KUBECONFIG=/path/to/cluster.kubeconfig
E2E_SEED_STORAGE_CLASS=fsx-openzfs \
E2E_CLONE_STORAGE_CLASS=fsx-openzfs-clone \
E2E_SNAPSHOT_CLASS=fsx-snap \
E2E_CSI_DRIVER=fsx.openzfs.csi.aws.com \
E2E_SIZING_MODE=sentinel \
E2E_SEED_SIZE=1Gi \
E2E_TIMEOUT_MULT=3 \
make test-e2e-external
```

`test-e2e-external` skips the kind/zpool bring-up and the operator deploy —
the suite drives whatever `KUBECONFIG` points at and expects the operator to
already be running (`volume-branch-operator-system`). Substitute your class
names; the values above match a typical FSx setup.

`E2E_TIMEOUT_MULT=3` stretches every wait: FSx serializes volume creation to
roughly one per minute, so on-demand clones and pool warm-up take minutes,
not seconds. (Which is also the pool's reason to exist — watch the claim
timing the suite prints and compare it with the on-demand branch.)

## After the run

The suite tears its objects down and asserts nothing engine-labeled
survives. Belt-and-braces for a billed environment:

```sh
aws fsx describe-volumes --query 'Volumes[].{id:VolumeId,name:Name,lifecycle:Lifecycle}'
```

should list only the filesystem's root volume (plus any volumes you created
yourself). Engine clones left in `CREATING`/`AVAILABLE` after a failed run
are leaks — deleting the seed `VolumeSnapshot`'s content and the leaked
child volumes from the FSx console/CLI is safe; the engine never touches the
root volume.
