//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/abuta-labs/volume-branch-operator/test/utils"
)

const (
	ns          = "vbo-e2e-app"
	seedPVC     = "seed-pvc"
	seedData    = "vbo-seed-1815"
	branchPVC   = "branch-pvc"
	branchWrite = "vbo-branch-write"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Driver seam: the same specs run against any snapshot-capable CSI backend.
// Defaults are the kind + ZFS-LocalPV harness values; an external run (e.g.
// the FSx gate) overrides them. sizingMode selects which sizing assertions
// apply — actual (requests track the discovered source size) or sentinel
// (fixed placeholder requests, for drivers that mandate an exact size).
var (
	seedSCName = envOr("E2E_SEED_STORAGE_CLASS", envOr("E2E_CLONE_STORAGE_CLASS", "vbo-e2e-zfs"))
	scName     = envOr("E2E_CLONE_STORAGE_CLASS", "vbo-e2e-zfs")
	vscName    = envOr("E2E_SNAPSHOT_CLASS", "vbo-e2e-zfs-snap")
	csiDriver  = envOr("E2E_CSI_DRIVER", "zfs.csi.openebs.io")
	sizingMode = envOr("E2E_SIZING_MODE", "actual")
	seedSize   = envOr("E2E_SEED_SIZE", "1200Mi")
	timeoutMul = func() float64 {
		m, err := strconv.ParseFloat(envOr("E2E_TIMEOUT_MULT", "1"), 64)
		if err != nil || m <= 0 {
			return 1
		}
		return m
	}()
)

// scaled stretches a base timeout by the backend multiplier (FSx serializes
// CreateVolume to roughly one per minute; kind-local ZFS clones are instant).
func scaled(base time.Duration) time.Duration {
	return time.Duration(float64(base) * timeoutMul)
}

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

func mustKubectl(args ...string) string {
	out, err := kubectl(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %s\n%s", strings.Join(args, " "), out)
	return out
}

func kubectlApply(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "apply failed: %s\n%s", manifest, out)
}

// runPod runs a one-shot pod against a PVC and waits for it to succeed.
// The pod is deleted afterwards so it never blocks PVC deletion.
func runPod(name, pvc, command string) string {
	kubectlApply(fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  containers:
  - name: main
    image: busybox:1.36
    command: ["sh", "-c", %q]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, name, ns, command, pvc))
	EventuallyWithOffset(1, func() string {
		out, _ := kubectl("-n", ns, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		return out
	}, scaled(3*time.Minute), 5*time.Second).Should(Equal("Succeeded"), "pod %s did not succeed", name)
	logs := mustKubectl("-n", ns, "logs", name)
	mustKubectl("-n", ns, "delete", "pod", name, "--wait=true")
	return logs
}

func branchPhase(name string) string {
	out, _ := kubectl("-n", ns, "get", "branch", name, "-o", "jsonpath={.status.phase}")
	return out
}

var _ = Describe("volume branching against "+csiDriver, Ordered, func() {
	var snapshotHandle, restoreSize string

	BeforeAll(func() {
		// A prior failed run leaves its cluster (and cluster-scoped engine
		// objects) behind for post-mortem; this suite must not inherit them —
		// a leftover warm pool would serve claims meant to test on-demand.
		_, _ = kubectl("delete", "branchpool", "--all", "--wait=true", "--timeout=120s")
		_, _ = kubectl("delete", "branchsource", "--all", "--wait=true", "--timeout=120s")
		_, _ = kubectl("delete", "namespace", ns, "--ignore-not-found", "--wait=true", "--timeout=120s")
		mustKubectl("create", "namespace", ns)
	})

	AfterAll(func() {
		_, _ = kubectl("delete", "namespace", ns, "--wait=false")
	})

	It("seeds a PVC with data", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, seedPVC, ns, seedSCName, seedSize))
		runPod("seed-writer", seedPVC, fmt.Sprintf("echo %s > /data/seed.txt && sync", seedData))
	})

	It("snapshots the seed and extracts the snapshot handle", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: seed-snap
  namespace: %s
spec:
  volumeSnapshotClassName: %s
  source:
    persistentVolumeClaimName: %s
`, ns, vscName, seedPVC))
		Eventually(func() string {
			out, _ := kubectl("-n", ns, "get", "volumesnapshot", "seed-snap",
				"-o", "jsonpath={.status.readyToUse}")
			return out
		}, scaled(3*time.Minute), 5*time.Second).Should(Equal("true"))

		bound := mustKubectl("-n", ns, "get", "volumesnapshot", "seed-snap",
			"-o", "jsonpath={.status.boundVolumeSnapshotContentName}")
		snapshotHandle = mustKubectl("get", "volumesnapshotcontent", bound,
			"-o", "jsonpath={.status.snapshotHandle}")
		Expect(snapshotHandle).NotTo(BeEmpty())
		restoreSize = strings.TrimSpace(mustKubectl("get", "volumesnapshotcontent", bound,
			"-o", "jsonpath={.status.restoreSize}"))
		if sizingMode == "actual" {
			// Actual-mode runs depend on size discovery downstream.
			Expect(restoreSize).NotTo(BeEmpty())
		}
	})

	It("takes a BranchSource to Ready", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.abuta-labs.com/v1alpha1
kind: BranchSource
metadata:
  name: e2e-source
spec:
  snapshotHandle: %q
  csiDriver: %s
  cloneStorageClassName: %s
  volumeSnapshotClassName: %s
`, snapshotHandle, csiDriver, scName, vscName))
		Eventually(func() string {
			out, _ := kubectl("get", "branchsource", "e2e-source", "-o", "jsonpath={.status.phase}")
			return out
		}, scaled(2*time.Minute), 3*time.Second).Should(Equal("Ready"))

		// The builtin profile for the driver under test must resolve to the
		// sizing mode this run asserts against.
		Expect(mustKubectl("get", "branchsource", "e2e-source",
			"-o", "jsonpath={.status.resolvedProfile.cloneSizeMode}")).To(Equal(sizingMode))
	})

	It("branches to a Bound PVC carrying the seed data", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.abuta-labs.com/v1alpha1
kind: Branch
metadata:
  name: br1
  namespace: %s
spec:
  source: e2e-source
  pvcName: %s
`, ns, branchPVC))
		Eventually(func() string { return branchPhase("br1") },
			scaled(4*time.Minute), 5*time.Second).Should(Equal("Ready"))
		Expect(mustKubectl("-n", ns, "get", "pvc", branchPVC,
			"-o", "jsonpath={.status.phase}")).To(Equal("Bound"))
		Expect(mustKubectl("-n", ns, "get", "branch", "br1",
			"-o", "jsonpath={.status.provisioning}")).To(Equal("ondemand"))

		logs := runPod("branch-reader", branchPVC, "cat /data/seed.txt")
		Expect(strings.TrimSpace(logs)).To(Equal(seedData))
	})

	It("discovers the source size from the originating snapshot content", func() {
		if sizingMode != "actual" {
			Skip("sentinel sizing: clone requests do not depend on size discovery")
		}
		// The source starts with only a snapshot handle — no size. The
		// engine finds the dynamically provisioned VolumeSnapshotContent
		// with the matching handle and reads its restoreSize; actual-mode
		// sizing keys off it from then on.
		wantQ := resource.MustParse(restoreSize)
		want := wantQ.Value()
		Eventually(func() int64 {
			out, _ := kubectl("get", "branchsource", "e2e-source", "-o", "jsonpath={.status.sizeBytes}")
			got, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
			return got
		}, scaled(2*time.Minute), 3*time.Second).Should(Equal(want),
			"source size never discovered from the originating VSC restoreSize")
	})

	It("isolates branch writes from the seed (CoW)", func() {
		runPod("branch-writer", branchPVC,
			fmt.Sprintf("echo %s > /data/branch.txt && sync", branchWrite))
		// The seed volume must not see the branch's write.
		logs := runPod("seed-reader", seedPVC,
			"ls /data; test ! -e /data/branch.txt && cat /data/seed.txt")
		Expect(logs).To(ContainSubstring(seedData))
		Expect(logs).NotTo(ContainSubstring("branch.txt"))
	})

	It("resets the branch back to the source state", func() {
		mustKubectl("-n", ns, "patch", "branch", "br1", "--type=merge",
			"-p", `{"spec":{"resetToken":"r1"}}`)
		// Phase drops out of Ready, then returns once the re-clone binds.
		Eventually(func() string { return branchPhase("br1") },
			scaled(2*time.Minute), 2*time.Second).ShouldNot(Equal("Ready"))
		Eventually(func() string { return branchPhase("br1") },
			scaled(4*time.Minute), 5*time.Second).Should(Equal("Ready"))
		Expect(mustKubectl("-n", ns, "get", "branch", "br1",
			"-o", "jsonpath={.status.observedResetToken}")).To(Equal("r1"))

		logs := runPod("reset-reader", branchPVC,
			"ls /data; test ! -e /data/branch.txt && cat /data/seed.txt")
		Expect(logs).To(ContainSubstring(seedData))
		Expect(logs).NotTo(ContainSubstring("branch.txt"))
	})

	It("reaps a TTL branch", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.abuta-labs.com/v1alpha1
kind: Branch
metadata:
  name: br-ttl
  namespace: %s
spec:
  source: e2e-source
  pvcName: ttl-pvc
  ttl: 30s
`, ns))
		Eventually(func() string { return branchPhase("br-ttl") },
			scaled(4*time.Minute), 5*time.Second).Should(Equal("Ready"))
		Eventually(func() string {
			out, _ := kubectl("-n", ns, "get", "branch", "br-ttl", "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out)
		}, scaled(3*time.Minute), 5*time.Second).Should(BeEmpty(), "TTL branch was never reaped")
		Eventually(func() string {
			out, _ := kubectl("-n", ns, "get", "pvc", "ttl-pvc", "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out)
		}, scaled(2*time.Minute), 5*time.Second).Should(BeEmpty(), "TTL branch PVC leaked")
	})

	It("warms a pool and claims from it by PV rebind", func() {
		kubectlApply(`
apiVersion: volumes.abuta-labs.com/v1alpha1
kind: BranchPool
metadata:
  name: e2e-pool
spec:
  source: e2e-source
  targetWarm: 1
`)
		// While the pool warms, its concurrency must never exceed the
		// profile's warming cap (FSx-class backends serialize CreateVolume;
		// flooding them is exactly what the cap prevents).
		capOut := mustKubectl("get", "branchsource", "e2e-source",
			"-o", "jsonpath={.status.resolvedProfile.maxWarmingDefault}")
		warmCap, err := strconv.Atoi(strings.TrimSpace(capOut))
		Expect(err).NotTo(HaveOccurred(), "unparsable maxWarmingDefault %q", capOut)
		Eventually(func() string {
			warming, _ := kubectl("get", "branchpool", "e2e-pool", "-o", "jsonpath={.status.warming}")
			if n, err := strconv.Atoi(strings.TrimSpace(warming)); err == nil {
				Expect(n).To(BeNumerically("<=", warmCap),
					"pool exceeded the profile warming cap")
			}
			out, _ := kubectl("get", "branchpool", "e2e-pool", "-o", "jsonpath={.status.warm}")
			return out
		}, scaled(4*time.Minute), 5*time.Second).Should(Equal("1"), "pool never warmed")

		// The warm clone's PV is the proof object: a pool claim must hand the
		// consumer THIS volume (rebind), not provision a new one.
		warmPV := strings.TrimSpace(mustKubectl("-n", "branch-pool", "get", "pvc",
			"-l", "volumes.abuta-labs.com/pool-state=warm",
			"-o", "jsonpath={.items[0].spec.volumeName}"))
		Expect(warmPV).NotTo(BeEmpty())
		// Compare as quantities: the PVC request canonicalizes ("2Gi") while
		// restoreSize is captured in raw bytes ("2147483648").
		warmReq := resource.MustParse(mustKubectl("-n", "branch-pool", "get", "pvc",
			"-l", "volumes.abuta-labs.com/pool-state=warm",
			"-o", "jsonpath={.items[0].spec.resources.requests.storage}"))
		if sizingMode == "actual" {
			wantWarm := resource.MustParse(restoreSize)
			Expect(warmReq.Value()).To(Equal(wantWarm.Value()),
				"warm clones must be actual-sized from the discovered source size")
		} else {
			sentinel := resource.MustParse("1Gi")
			Expect(warmReq.Value()).To(Equal(sentinel.Value()),
				"sentinel mode: warm clones must request exactly the sentinel size")
		}

		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.abuta-labs.com/v1alpha1
kind: Branch
metadata:
  name: br-pool
  namespace: %s
spec:
  source: e2e-source
  pvcName: pool-clone-pvc
`, ns))
		start := time.Now()
		Eventually(func() string { return branchPhase("br-pool") },
			scaled(2*time.Minute), 2*time.Second).Should(Equal("Ready"))
		claimTime := time.Since(start)
		GinkgoWriter.Printf("pool claim to Ready: %s\n", claimTime)

		Expect(mustKubectl("-n", ns, "get", "branch", "br-pool",
			"-o", "jsonpath={.status.provisioning}")).To(Equal("pool"))
		gotPV := strings.TrimSpace(mustKubectl("-n", ns, "get", "pvc", "pool-clone-pvc",
			"-o", "jsonpath={.spec.volumeName}"))
		Expect(gotPV).To(Equal(warmPV), "claim must rebind the pre-warmed PV, not provision")

		logs := runPod("pool-reader", "pool-clone-pvc", "cat /data/seed.txt")
		Expect(strings.TrimSpace(logs)).To(Equal(seedData))
	})

	It("replenishes the pool after the claim", func() {
		Eventually(func() string {
			out, _ := kubectl("get", "branchpool", "e2e-pool", "-o", "jsonpath={.status.warm}")
			return out
		}, scaled(4*time.Minute), 5*time.Second).Should(Equal("1"), "pool never replenished")
		Expect(mustKubectl("get", "branchpool", "e2e-pool",
			"-o", "jsonpath={.status.claimedTotal}")).To(Equal("1"))
	})

	It("tears down the pool-claimed branch including its PV", func() {
		claimedPV := strings.TrimSpace(mustKubectl("-n", ns, "get", "pvc", "pool-clone-pvc",
			"-o", "jsonpath={.spec.volumeName}"))
		mustKubectl("-n", ns, "delete", "branch", "br-pool", "--wait=true", "--timeout=180s")
		Eventually(func() string {
			out, _ := kubectl("get", "pv", claimedPV, "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out)
		}, scaled(2*time.Minute), 5*time.Second).Should(BeEmpty(), "claimed PV leaked after branch teardown")
	})

	It("tears down the pool's warm set on pool delete", func() {
		mustKubectl("delete", "branchpool", "e2e-pool", "--wait=true", "--timeout=180s")
		Eventually(func() string {
			out, _ := kubectl("-n", "branch-pool", "get", "pvc", "-o", "name")
			return strings.TrimSpace(out)
		}, scaled(2*time.Minute), 5*time.Second).Should(BeEmpty(), "warm clones leaked after pool delete")
	})

	It("tears everything down without leaks", func() {
		mustKubectl("-n", ns, "delete", "branch", "br1", "--wait=true", "--timeout=180s")
		Expect(mustKubectl("-n", ns, "get", "pvc", "-o", "name")).NotTo(ContainSubstring(branchPVC))

		mustKubectl("delete", "branchsource", "e2e-source", "--wait=true", "--timeout=180s")
		// No engine-labeled objects may survive source teardown.
		out := mustKubectl("get", "volumesnapshotcontent",
			"-l", "volumes.abuta-labs.com/source", "-o", "name")
		Expect(strings.TrimSpace(out)).To(BeEmpty(), "leaked VSCs: %s", out)
	})
})
