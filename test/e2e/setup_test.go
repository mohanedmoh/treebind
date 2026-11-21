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
	"strconv"
	"strings"
	"time"

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

// syncTarget names what a spec syncs and where it looks: a Treebind and its namespace, the pod of the application that mounts the
// claim, and the bare repository on the ssh remote with the clone that pushes to it as a person with Git does. The definition of done
// has one (dod), and each failure spec has its own repository, claim and Treebind, so that one that fails leaves nothing to the next.
type syncTarget struct {
	namespace, name string
	// appPod mounts the claim at appMount, with the subPath of the Treebind.
	appPod, appMount string
	// repository is the path of the bare repository in the pod of the remote, and branch the branch of the Treebind. clone is the path
	// of the clone in the pod of the remote that seeded the repository, which the pushes from Git use as well.
	repository, branch, clone string
}

// syncPodName is the pod of the StatefulSet that the operator makes for the Treebind.
func (t syncTarget) syncPodName() string { return t.name + "-sync-0" }

// remoteFiles lists the files of the branch of the repository of the remote.
func (t syncTarget) remoteFiles(g Gomega) []string {
	output, err := remoteShell("git -C " + t.repository + " ls-tree -r --name-only " + t.branch)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to list the files of the repository")
	return utils.GetNonEmptyLines(output)
}

// remoteTip is the full id of the tip of the branch of the repository of the remote.
func (t syncTarget) remoteTip(g Gomega) string {
	output, err := remoteShell("git -C " + t.repository + " rev-parse --verify refs/heads/" + t.branch)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the tip of the repository")
	return output
}

// pushFromGit makes a commit with a file in the clone that the seeding made, after taking what the repository has, and pushes it.
func (t syncTarget) pushFromGit(name, content string) {
	_, err := remoteShell("cd " + t.clone + " && git pull -q " + t.repository + " " + t.branch + " && echo " + shellQuote(content) +
		" > " + shellQuote(name) + " && git add -A && git commit -q -m " + shellQuote("from git: "+name) + " && git push -q " +
		t.repository + " " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to push from Git")
}

// appWrite writes a file in the directory of the claim that the pod of the application mounts, as the application does.
func (t syncTarget) appWrite(name, content string) {
	_, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "sh", "-c",
		"echo "+shellQuote(content)+" > "+shellQuote(t.appMount+"/"+name)))
	Expect(err).NotTo(HaveOccurred(), "Failed to write a file in the pod of the application")
}

// appFile is the content of a file in the pod of the application, without the newline at the end.
func (t syncTarget) appFile(g Gomega, name string) string {
	output, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "cat", t.appMount+"/"+name))
	g.Expect(err).NotTo(HaveOccurred(), "The file is not in the pod of the application yet")
	return strings.TrimSpace(output)
}

// treebindSyncState is what one read of the status of the Treebind says of the sync: the conditions that the operator writes from the
// report of the agent, with their reasons, and the two fields of the status. A condition or a field that is not there is empty.
type treebindSyncState struct {
	ready, synced, syncedReason, conflict, conflictReason, lastSyncedCommit, lastSyncTime string
}

// sync reads the status of the Treebind once, so that what it says of the sync is of one moment.
func (t syncTarget) sync(g Gomega) treebindSyncState {
	condition := func(conditionType, field string) string {
		return `{.status.conditions[?(@.type=="` + conditionType + `")].` + field + `}`
	}
	fields := []string{
		condition("Ready", "status"),
		condition("Synced", "status"), condition("Synced", "reason"),
		condition("Conflict", "status"), condition("Conflict", "reason"),
		"{.status.lastSyncedCommit}", "{.status.lastSyncTime}",
	}
	output, err := utils.Run(exec.Command("kubectl", "get", "treebind", t.name, "-n", t.namespace,
		"-o", "jsonpath="+strings.Join(fields, "|")))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the Treebind")
	values := strings.Split(strings.TrimSpace(output), "|")
	g.Expect(values).To(HaveLen(len(fields)), "kubectl answered %q", output)
	return treebindSyncState{
		ready: values[0], synced: values[1], syncedReason: values[2], conflict: values[3], conflictReason: values[4],
		lastSyncedCommit: values[5], lastSyncTime: values[6],
	}
}

// expectSyncedAtTip expects the status of the Treebind to say, in one read, that the volume and the repository agree at the tip of
// the remote, and returns that tip: Synced is True with the reason InSync, status.lastSyncedCommit is the full id of the tip, and
// status.lastSyncTime is not before the time given, which is the time before the change that made the tip, because the agreement
// at a tip is seen after it was made. Conflict is False, with the reason NoConflict or, when an earlier spec had the agent resolve a
// conflict, ConflictResolved, and Ready is True.
func (t syncTarget) expectSyncedAtTip(g Gomega, notBefore time.Time) string {
	state := t.sync(g)
	tip := t.remoteTip(g)
	g.Expect(state.synced).To(Equal("True"), "Synced is %q with the reason %q", state.synced, state.syncedReason)
	g.Expect(state.syncedReason).To(Equal("InSync"))
	g.Expect(state.lastSyncedCommit).To(Equal(tip), "status.lastSyncedCommit is not the tip of the remote")
	// The operator keeps the time in whole seconds, as the API server does.
	agreed, err := time.Parse(time.RFC3339, state.lastSyncTime)
	g.Expect(err).NotTo(HaveOccurred(), "status.lastSyncTime is %q", state.lastSyncTime)
	g.Expect(agreed).To(BeTemporally(">=", notBefore.Truncate(time.Second)), "status.lastSyncTime is older than the change")
	g.Expect(state.conflict).To(Equal("False"), "Conflict is %q with the reason %q", state.conflict, state.conflictReason)
	// The Conflict check keeps the last resolution since the agent started, and the specs before this one write on both sides close
	// together, so that a pull may have resolved a conflict: what matters is that no conflict waits.
	g.Expect(state.conflictReason).To(Or(Equal("NoConflict"), Equal("ConflictResolved")))
	g.Expect(state.ready).To(Equal("True"), "Ready is %q", state.ready)
	return tip
}

// syncPodState is what one read of the pod of the sync says of it.
type syncPodState struct {
	// uid is empty when there is no such pod.
	uid string
	// ready is whether its container is ready, and terminating whether it is being deleted.
	ready, terminating bool
	// restarts is how many times the kubelet started its container again, and started when the container that runs now started.
	restarts int
	started  time.Time
}

// syncPod reads the pod of the sync once, so that what it says is of the same pod and the same container.
func (t syncTarget) syncPod(g Gomega) syncPodState {
	output, err := utils.Run(exec.Command("kubectl", "get", "pod", t.syncPodName(), "-n", t.namespace, "--ignore-not-found",
		"-o", "jsonpath={.metadata.uid}|{.status.containerStatuses[0].ready}|{.metadata.deletionTimestamp}|"+
			"{.status.containerStatuses[0].restartCount}|{.status.containerStatuses[0].state.running.startedAt}"))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the sync")
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) < 5 {
		// There is no such pod, and kubectl printed nothing.
		return syncPodState{}
	}
	state := syncPodState{uid: fields[0], ready: fields[1] == "true", terminating: fields[2] != ""}
	if fields[3] != "" {
		state.restarts, err = strconv.Atoi(fields[3])
		g.Expect(err).NotTo(HaveOccurred(), "the restart count is %q", fields[3])
	}
	if fields[4] != "" {
		state.started, err = time.Parse(time.RFC3339, fields[4])
		g.Expect(err).NotTo(HaveOccurred(), "the container started at %q", fields[4])
	}
	return state
}
