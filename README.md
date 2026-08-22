# volume-branch-operator

A Kubernetes operator for **volume branching**: instant copy-on-write clones of stateful
volumes, kept warm in pools so a branch is ready in seconds instead of your storage
backend's CreateVolume latency — over any snapshot-capable CSI driver.

## The problem

CSI gives Kubernetes the primitives — `VolumeSnapshot`, snapshot-sourced PVCs — but
nothing orchestrates *branch lifecycle* on top of them:

- a named, immutable **source** to branch from
- N **pre-warmed clones** waiting in a pool, so claiming a branch is a label flip, not a
  slow CreateVolume call
- atomic **claim** into a target namespace, **reset** (re-branch from the same source),
  and ordered **reclaim** that respects backend quirks (snapshot-pins-volume rules,
  clone-rate limits, sizing modes)

Database-level cloning tools (CNPG, Zalando) solve this per-engine. This operator solves
it once, at the volume layer, for anything that runs on a PVC.

This is **not** a storage backend selector — `storageClassName` already does that — and
not a CSI driver. It orchestrates standard snapshot objects over the drivers you already
run.

## CRDs

| Kind | Purpose |
|---|---|
| `BranchSource` | an immutable snapshot handle to branch from |
| `BranchPool` | a warm set of pre-cloned volumes against a source |
| `Branch` | one claimed clone, terminating at a Bound PVC in your namespace |

## Status

**Pre-release — under construction.** The design is proven in production by the operator behind
[Adjoint](https://adjoint.sh), which runs branchable
Postgres environments on AWS FSx for OpenZFS; this repo is a standalone, generalized
build of that engine. Until the e2e
suite passes on two CSI drivers, expect the API to change without notice.

## License

Apache-2.0
