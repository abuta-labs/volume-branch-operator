# Walk-through: OpenEBS ZFS-LocalPV (proven driver)

This is the complete flow on the driver the engine's e2e suite runs against:
seed a volume, snapshot it, declare the snapshot a `BranchSource`, keep a warm
pool against it, and claim `Branch`es. It matches what `hack/e2e-up.sh` sets
up in kind, so you can run it verbatim there.

Prereqs: a zpool on the node(s) (the walk-through assumes it is named
`zfspv-pool`), [ZFS-LocalPV](https://github.com/openebs/zfs-localpv)
installed, the external-snapshotter CRDs + snapshot-controller present, and
the operator deployed (`make deploy`).

Apply in order:

1. `00-classes.yaml` — StorageClass + VolumeSnapshotClass for the driver.
2. Create a seed PVC and put data on it (your database, fixture set, …).
3. Snapshot the seed PVC (`01-seed-snapshot.yaml`) and read the handle:

   ```sh
   kubectl get volumesnapshotcontent \
     -o jsonpath='{.items[?(@.spec.volumeSnapshotRef.name=="seed-snap")].status.snapshotHandle}'
   ```

4. Put that handle into `10-branchsource.yaml` and apply it. Keep the
   snapshot object around: the source's size is discovered from its
   originating VolumeSnapshotContent (ZFS-LocalPV enforces
   request >= restore size, so the engine's `actual` sizing needs it —
   otherwise declare `spec.sizeBytes`).
5. `20-branchpool.yaml` — optional warm pool; claims become label flips.
6. `30-branch.yaml` — a Branch per consumer; each ends as a Bound PVC.

Check progress at any point:

```sh
kubectl get branchsources,branchpools
kubectl get branches -A
bin/manager verify   # read-only preflight
```
