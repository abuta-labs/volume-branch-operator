#!/usr/bin/env bash
# e2e-down: delete the kind cluster. The zpool and its backing file live
# inside the node container, so they disappear with it — nothing to clean on
# the host.
set -euo pipefail
KIND_CLUSTER="${KIND_CLUSTER:-volume-branch-operator-test-e2e}"
KIND="${KIND:-kind}"
POOL="${E2E_ZPOOL:-vbo-e2e}"
NODE="${KIND_CLUSTER}-control-plane"

# Destroy the pool and detach its loop device first — the loop device is a
# host-global resource and would leak past the cluster's lifetime otherwise.
docker exec "$NODE" bash -c "
  zpool destroy -f '$POOL' 2>/dev/null || true
  for l in \$(losetup -j /var/${POOL}.img -O NAME --noheadings 2>/dev/null); do
    losetup -d \"\$l\" || true
  done
" 2>/dev/null || true

"$KIND" delete cluster --name "$KIND_CLUSTER"
