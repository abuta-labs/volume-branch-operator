#!/usr/bin/env bash
# e2e-up: stand up a kind cluster with OpenEBS ZFS-LocalPV as the CSI driver.
#
# ZFS state is kernel-level, and kind nodes are privileged containers sharing
# the host kernel — so the zpool is created INSIDE the kind node from a plain
# file vdev. The host only needs the zfs kernel module loaded; the userland
# tools are installed in the node container, and the pool (plus its backing
# file) vanishes with the node on teardown. No host root required.
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-volume-branch-operator-test-e2e}"
KIND="${KIND:-kind}"
POOL="${E2E_ZPOOL:-vbo-e2e}"
POOL_SIZE="${E2E_ZPOOL_SIZE:-2G}"
SNAPSHOTTER_VERSION="v8.2.0"
ZFS_LOCALPV_VERSION="v2.8.0"
NODE="${KIND_CLUSTER}-control-plane"
HERE="$(cd "$(dirname "$0")" && pwd)"

need() { command -v "$1" >/dev/null 2>&1 || { echo "ERROR: '$1' is required" >&2; exit 1; }; }
need docker; need "$KIND"; need kubectl

if [ ! -d /sys/module/zfs ]; then
  echo "ERROR: the zfs kernel module is not loaded on the host." >&2
  echo "Run: sudo apt-get install -y zfsutils-linux && sudo modprobe zfs" >&2
  exit 1
fi

case "$("$KIND" get clusters 2>/dev/null)" in
  *"$KIND_CLUSTER"*) echo "kind cluster '$KIND_CLUSTER' already exists" ;;
  *) "$KIND" create cluster --name "$KIND_CLUSTER" --wait 120s ;;
esac

echo ">> zfs userland + pool inside the kind node"
docker exec "$NODE" bash -euc "
  command -v zpool >/dev/null 2>&1 || {
    # kind node images are Debian; zfs userland lives in the contrib component.
    sed -i 's/^Components: main$/Components: main contrib/' /etc/apt/sources.list.d/debian.sources
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends zfsutils-linux >/dev/null
  }
  zpool list '$POOL' >/dev/null 2>&1 || {
    truncate -s '$POOL_SIZE' /var/${POOL}.img
    # The ZFS kernel module resolves vdev paths in the kernel's namespace,
    # where the container's overlayfs file is invisible — a plain file vdev
    # fails with 'no such pool or dataset'. A loop device is a global block
    # device the kernel can open, so wrap the file in one.
    # The container's /dev is a static snapshot; the kernel's first FREE
    # loop number can exceed the nodes present here (other host workloads
    # hold the low ones). Create the missing node before use.
    LOOPDEV=\$(losetup -f)
    [ -e "\$LOOPDEV" ] || mknod "\$LOOPDEV" b 7 "\${LOOPDEV#/dev/loop}"
    LOOP=\$(losetup -f --show /var/${POOL}.img)
    zpool create -f '$POOL' "\$LOOP"
  }
  zpool list '$POOL'
"

echo ">> external-snapshotter CRDs + snapshot-controller ($SNAPSHOTTER_VERSION)"
BASE="https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/${SNAPSHOTTER_VERSION}"
kubectl apply -f "$BASE/client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml" \
              -f "$BASE/client/config/crd/snapshot.storage.k8s.io_volumesnapshotcontents.yaml" \
              -f "$BASE/client/config/crd/snapshot.storage.k8s.io_volumesnapshots.yaml"
kubectl apply -f "$BASE/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml" \
              -f "$BASE/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"

echo ">> OpenEBS ZFS-LocalPV ($ZFS_LOCALPV_VERSION)"
kubectl apply -f "https://raw.githubusercontent.com/openebs/zfs-localpv/${ZFS_LOCALPV_VERSION}/deploy/zfs-operator.yaml"

echo ">> waiting for the storage stack"
kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=180s
kubectl -n kube-system rollout status deploy/openebs-zfs-localpv-controller --timeout=300s
kubectl -n kube-system rollout status ds/openebs-zfs-localpv-node --timeout=300s

echo ">> StorageClass + VolumeSnapshotClass (pool=$POOL)"
sed "s/POOLNAME/$POOL/" "$HERE/../test/e2e/manifests/storage.yaml" | kubectl apply -f -

echo "e2e environment ready (cluster=$KIND_CLUSTER pool=$POOL)"
