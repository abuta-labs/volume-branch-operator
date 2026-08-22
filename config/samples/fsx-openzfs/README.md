# Walk-through: Amazon FSx for OpenZFS — proven (gate passed 2026-08-22)

The engine ships a built-in profile for `fsx.openzfs.csi.aws.com`
(snapshot-pins-volume teardown ordering, sentinel sizing, low warming
concurrency — FSx serializes CreateVolume backend-side), but the FSx
end-to-end suite has NOT run yet. Treat this walk-through as a sketch to
adapt, not a recipe that has been executed. The proven path today is
ZFS-LocalPV (../zfs-localpv/).

Prereqs: an FSx for OpenZFS filesystem reachable from your cluster's VPC, the
[aws-fsx-openzfs-csi-driver](https://github.com/kubernetes-sigs/aws-fsx-openzfs-csi-driver)
installed, snapshotter CRDs + snapshot-controller, the operator deployed.

Flow is identical to zfs-localpv: classes → seed volume + snapshot → handle
into a BranchSource → pool → branches. FSx-specific notes:

- The StorageClass provisions FSx *child volumes* under a parent volume —
  `ParentVolumeId` must point at an existing volume on your filesystem.
- Clone PVC capacity requests are placeholders on FSx (clones are full-size
  views of their parent); the built-in profile therefore uses sentinel
  sizing. Do not switch it to `actual`.
- A warm pool matters here: FSx serializes volume creation (about one per
  minute) — this is exactly the backend the pool exists for.
