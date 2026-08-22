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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/arbit-tech/volume-branch-operator/test/utils"
)

const (
	ns          = "vbo-e2e-app"
	scName      = "vbo-e2e-zfs"
	vscName     = "vbo-e2e-zfs-snap"
	seedPVC     = "seed-pvc"
	seedData    = "vbo-seed-1815"
	branchPVC   = "branch-pvc"
	branchWrite = "vbo-branch-write"
)

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
	}, 3*time.Minute, 5*time.Second).Should(Equal("Succeeded"), "pod %s did not succeed", name)
	logs := mustKubectl("-n", ns, "logs", name)
	mustKubectl("-n", ns, "delete", "pod", name, "--wait=true")
	return logs
}

func branchPhase(name string) string {
	out, _ := kubectl("-n", ns, "get", "branch", name, "-o", "jsonpath={.status.phase}")
	return out
}

var _ = Describe("volume branching on ZFS-LocalPV", Ordered, func() {
	var snapshotHandle string

	BeforeAll(func() {
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
      storage: 1Gi
`, seedPVC, ns, scName))
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
		}, 3*time.Minute, 5*time.Second).Should(Equal("true"))

		bound := mustKubectl("-n", ns, "get", "volumesnapshot", "seed-snap",
			"-o", "jsonpath={.status.boundVolumeSnapshotContentName}")
		snapshotHandle = mustKubectl("get", "volumesnapshotcontent", bound,
			"-o", "jsonpath={.status.snapshotHandle}")
		Expect(snapshotHandle).NotTo(BeEmpty())
	})

	It("takes a BranchSource to Ready", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.arbit-tech.com/v1alpha1
kind: BranchSource
metadata:
  name: e2e-source
spec:
  snapshotHandle: %q
  csiDriver: zfs.csi.openebs.io
  cloneStorageClassName: %s
  volumeSnapshotClassName: %s
`, snapshotHandle, scName, vscName))
		Eventually(func() string {
			out, _ := kubectl("get", "branchsource", "e2e-source", "-o", "jsonpath={.status.phase}")
			return out
		}, 2*time.Minute, 3*time.Second).Should(Equal("Ready"))
	})

	It("branches to a Bound PVC carrying the seed data", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.arbit-tech.com/v1alpha1
kind: Branch
metadata:
  name: br1
  namespace: %s
spec:
  source: e2e-source
  pvcName: %s
`, ns, branchPVC))
		Eventually(func() string { return branchPhase("br1") },
			4*time.Minute, 5*time.Second).Should(Equal("Ready"))
		Expect(mustKubectl("-n", ns, "get", "pvc", branchPVC,
			"-o", "jsonpath={.status.phase}")).To(Equal("Bound"))
		Expect(mustKubectl("-n", ns, "get", "branch", "br1",
			"-o", "jsonpath={.status.provisioning}")).To(Equal("ondemand"))

		logs := runPod("branch-reader", branchPVC, "cat /data/seed.txt")
		Expect(strings.TrimSpace(logs)).To(Equal(seedData))
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
			2*time.Minute, 2*time.Second).ShouldNot(Equal("Ready"))
		Eventually(func() string { return branchPhase("br1") },
			4*time.Minute, 5*time.Second).Should(Equal("Ready"))
		Expect(mustKubectl("-n", ns, "get", "branch", "br1",
			"-o", "jsonpath={.status.observedResetToken}")).To(Equal("r1"))

		logs := runPod("reset-reader", branchPVC,
			"ls /data; test ! -e /data/branch.txt && cat /data/seed.txt")
		Expect(logs).To(ContainSubstring(seedData))
		Expect(logs).NotTo(ContainSubstring("branch.txt"))
	})

	It("reaps a TTL branch", func() {
		kubectlApply(fmt.Sprintf(`
apiVersion: volumes.arbit-tech.com/v1alpha1
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
			4*time.Minute, 5*time.Second).Should(Equal("Ready"))
		Eventually(func() string {
			out, _ := kubectl("-n", ns, "get", "branch", "br-ttl", "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out)
		}, 3*time.Minute, 5*time.Second).Should(BeEmpty(), "TTL branch was never reaped")
		Eventually(func() string {
			out, _ := kubectl("-n", ns, "get", "pvc", "ttl-pvc", "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 5*time.Second).Should(BeEmpty(), "TTL branch PVC leaked")
	})

	It("tears everything down without leaks", func() {
		mustKubectl("-n", ns, "delete", "branch", "br1", "--wait=true", "--timeout=180s")
		Expect(mustKubectl("-n", ns, "get", "pvc", "-o", "name")).NotTo(ContainSubstring(branchPVC))

		mustKubectl("delete", "branchsource", "e2e-source", "--wait=true", "--timeout=180s")
		// No engine-labeled objects may survive source teardown.
		out := mustKubectl("get", "volumesnapshotcontent",
			"-l", "volumes.arbit-tech.com/source", "-o", "name")
		Expect(strings.TrimSpace(out)).To(BeEmpty(), "leaked VSCs: %s", out)
	})
})
