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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/abuta-labs/volume-branch-operator/test/utils"
)

// managerImage is built locally and side-loaded into kind — never pulled.
var managerImage = "example.com/volume-branch-operator:e2e"

// TestE2E validates the full branch lifecycle against a real CSI driver
// (OpenEBS ZFS-LocalPV) in the kind cluster prepared by hack/e2e-up.sh.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting volume-branch-operator e2e suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// External mode (E2E_SKIP_DEPLOY=1): the operator is already installed on
	// the target cluster (e.g. from a release install.yaml) and KUBECONFIG
	// points at it — no kind, no image build, no deploy. Used by the FSx gate.
	if os.Getenv("E2E_SKIP_DEPLOY") != "" {
		By("external cluster: waiting for the deployed operator to be ready")
		cmd := exec.Command("kubectl", "-n", "volume-branch-operator-system", "rollout",
			"status", "deploy/volume-branch-operator-controller-manager", "--timeout=180s")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "operator not ready on the external cluster")
		return
	}

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image into kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())

	By("deploying the operator")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the operator")

	// The image tag is constant, so on a reused cluster (a prior failed run
	// leaves it up) an unchanged manifest triggers no rollout and the OLD
	// binary keeps running — a freshly kind-loaded image is only picked up
	// by a restart. Cost on a fresh cluster: one extra pod start.
	cmd = exec.Command("kubectl", "-n", "volume-branch-operator-system", "rollout",
		"restart", "deploy/volume-branch-operator-controller-manager")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to restart the operator")

	cmd = exec.Command("kubectl", "-n", "volume-branch-operator-system", "rollout",
		"status", "deploy/volume-branch-operator-controller-manager", "--timeout=180s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "operator never became ready")
})

var _ = AfterSuite(func() {
	// The kind cluster is deleted by make cleanup-test-e2e; nothing to do.
})
