//go:build e2e
// +build e2e

/*
Copyright The Treebind Authors.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mohanedmoh/treebind/test/utils"
)

// failureLabel is the Ginkgo label of the specs that cause a failure on purpose: they kill the agent, take the remote away or fill
// a volume. They take longer than the others and need more of the cluster, so `make test-e2e` leaves them out
// (-ginkgo.label-filter='!failure') and `make test-e2e-failures` runs only them (-ginkgo.label-filter=failure), each on a kind
// cluster of its own. A spec or a Context that causes a failure is declared with Label(failureLabel).
const failureLabel = "failure"

// installAgentImage builds the sync agent image with ko into the cluster, sets it as AGENT_IMAGE on the deployment of the operator
// and waits for the operator to roll out with it and for all of its pods to be ready. It needs the operator to be deployed, which
// the BeforeAll of the Describe of the manager does, ko, and the Kind cluster that KIND_CLUSTER names. It leaves agentImage set to
// the reference that the cluster knows the image by, and the operator running with it: the sync pods that it makes from now on run
// the real agent. Setting the environment replaces the pods of the operator, so it comes after the specs that need the pods as they
// were.
func installAgentImage() {
	By("building the sync agent image with ko and loading it on Kind")
	var err error
	agentImage, err = utils.BuildAgentImage()
	Expect(err).NotTo(HaveOccurred(), "Failed to build the sync agent image")
	_, _ = fmt.Fprintf(GinkgoWriter, "sync agent image: %s\n", agentImage)

	By("giving the operator the agent image")
	_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "set", "env",
		"deployment/treebind-controller-manager", "AGENT_IMAGE="+agentImage))
	Expect(err).NotTo(HaveOccurred(), "Failed to set the agent image on the operator")
	_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "status",
		"deployment/treebind-controller-manager", "--timeout=180s"))
	Expect(err).NotTo(HaveOccurred(), "The operator did not roll out with the agent image")
	Eventually(func(g Gomega) {
		g.Expect(readyControllerPods(g)).To(HaveLen(controllerReplicas))
	}).Should(Succeed())
}

// startRemoteFor makes the namespace ns and runs hack/dev-ssh.sh for it, which starts the ssh remote of docs/development.md in the
// namespace treebind-dev (remoteNamespace) and makes the Secret dev-deploy-key and the ConfigMap dev-known-hosts in ns. It needs
// kubectl, ssh-keygen and the network for the image of the remote, and ns must not exist. It leaves the namespace, the key and the
// host keys in place and the remote running, with an empty bare repository at remoteRepository, which is for the caller to seed.
// The script makes a new key each time it runs, and restarts a remote that is running, which loses its repository and its host key
// (the repository is in an emptyDir). So a second call, for another namespace, takes the remote away from the namespaces of the
// calls before it. Removing the namespace and the remote is for the caller.
func startRemoteFor(ns string) {
	By("making the namespace of the Treebind")
	_, err := utils.Run(exec.Command("kubectl", "create", "namespace", ns))
	Expect(err).NotTo(HaveOccurred(), "Failed to create the namespace")

	By("starting the ssh remote, with a deploy key and the host key")
	_, err = utils.Run(exec.Command("hack/dev-ssh.sh", ns))
	Expect(err).NotTo(HaveOccurred(), "Failed to start the ssh remote")
}
