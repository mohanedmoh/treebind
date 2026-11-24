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
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mohanedmoh/treebind/test/utils"
)

// The specs of #55 cause a failure on purpose and check how the sync comes back from it, with the real agent and the ssh remote, as
// the definition of done does. They share one namespace with the key of the remote, because hack/dev-ssh.sh makes a new key and
// restarts the remote, which loses its repositories, each time it runs. Each failure has its own repository on the remote, its own
// claim and its own Treebind, and removes them when it ends, so that one that fails leaves nothing to the next. hack/inject.sh causes
// what the cluster cannot: it kills or freezes the agent from the node, through a privileged pod that the specs remove at the end.
const (
	failureNamespace = "treebind-failures-e2e"
	// failureAgentContainer is the container of the agent in the pod of the sync.
	failureAgentContainer = "agent"
	failureSeedFile       = "hello.txt"

	// failureLockTTL is the TTL of the writer lock of the agent (internal/agent/app.DefaultLockTTL), which no setting changes. A
	// container that was killed did not give the lock up, so the one that the kubelet starts in its place waits this long for it.
	failureLockTTL = 30 * time.Second
	// failureKillBound is how long the pod has, after its agent was killed, to be ready again: the kubelet starts the container again,
	// the new agent waits a TTL for the lock, reopens the checkout and pulls. On kind the container was started again within a second
	// of the kill and the pod was ready after 33 seconds. The kubelet waits longer before each start of a container that it started
	// again a short while before, and a spec kills the agent of a pod once, so that the bound has room for a slow node and not for that.
	failureKillBound = 3 * time.Minute
)

// remoteSSH is the ssh remote of hack/dev-ssh.sh, as a Treebind names it, without the path of a repository.
const remoteSSH = "ssh://git@" + remoteDeployment + "." + remoteNamespace + ".svc.cluster.local"

// lockLine is the line of the log in which the agent says that it took the writer lock: the result of the check Lock, with the
// epoch of the tenure, which is higher than the epoch of every holder before it (internal/agent/lock).
var lockLine = regexp.MustCompile(`(?m)^time=(\S+) level=INFO msg="writer lock" check=Lock outcome=Passed .*from epoch (\d+)`)

// failureTarget is what a failure spec syncs, named after the failure, in the namespace that the failure specs share.
func failureTarget(name string) syncTarget {
	return syncTarget{
		namespace: failureNamespace, name: name, appPod: name + "-app", appMount: "/data",
		repository: "/srv/" + name + ".git", branch: "main", clone: "/tmp/seed-" + name,
	}
}

// failureManifest is the claim, the Treebind and the pod of the application of a failure spec, as the definition of done has them.
func failureManifest(t syncTarget) string {
	return `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ` + t.name + `-data
  namespace: ` + t.namespace + `
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 100Mi
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: ` + t.name + `
  namespace: ` + t.namespace + `
spec:
  repository:
    url: ` + remoteSSH + t.repository + `
    branch: ` + t.branch + `
    auth:
      sshKeySecretRef: {name: dev-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: dev-known-hosts, key: known_hosts}
  target:
    claimName: ` + t.name + `-data
    subPath: ` + dodSubPath + `
  sync:
    direction: bidirectional
    interval: ` + dodInterval + `
    settleTime: ` + dodSettleTime + `
---
apiVersion: v1
kind: Pod
metadata:
  name: ` + t.appPod + `
  namespace: ` + t.namespace + `
spec:
  containers:
  - name: app
    image: ` + dodAppImage + `
    command: ["sleep", "3600"]
    volumeMounts:
    - {name: data, mountPath: ` + t.appMount + `, subPath: ` + dodSubPath + `}
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: ` + t.name + `-data
`
}

// failureInjection is the Context of the failure specs. It is called from the Describe of the manager after the definition of done,
// with the label failure, so that make test-e2e-failures runs it alone and make test-e2e leaves it out.
func failureInjection() {
	BeforeAll(func() {
		// The definition of done gives the operator the agent image when it runs in the same run, and a run of the label failure
		// alone does not run it.
		if agentImage == "" {
			installAgentImage()
		}
		startRemoteFor(failureNamespace)
	})

	AfterAll(func() {
		By("removing the privileged pod of hack/inject.sh, the namespace of the failures and the remote")
		if err := utils.RemoveInjector(); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
		}
		for _, ns := range []string{failureNamespace, remoteNamespace} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", ns, "--ignore-not-found", "--timeout=3m"))
		}
	})

	Context("when the agent is killed between two steps", func() {
		target := failureTarget("kill")
		BeforeAll(func() { setUpFailure(target) })
		AfterAll(func() { tearDownFailure(target) })
		AfterEach(func() { diagnoseFailure(target) })

		It("should start it again in the same pod, which takes the writer lock over and syncs both ways", func() {
			killBetweenSteps(target)
		})
	})

	killMidStep()

	Context("when the volume is full and is freed again", fullVolume)

	Context("when the remote is taken away and brought back", remoteDown)

	Context("when the agent that holds the writer lock is frozen under a second one", frozenHolder)
}

// killBetweenSteps kills the agent of a Treebind that has nothing to do, as an out-of-memory kill does, while the application writes a
// file and a commit is pushed from Git, and checks that the kubelet starts it again once in the same pod, that the new agent takes
// the writer lock after a TTL with a higher epoch and reopens the checkout, and that both changes arrive and the status says so.
func killBetweenSteps(t syncTarget) {
	By("waiting for the Treebind to be ready, with the seeded file in the pod of the application")
	Eventually(func(g Gomega) {
		g.Expect(t.sync(g).ready).To(Equal("True"))
		g.Expect(t.appFile(g, failureSeedFile)).To(Equal("hello"))
	}, dodRestartBound).Should(Succeed())

	// Between two steps is when the agent has nothing to do: the status says that the volume and the repository agree at the tip,
	// and no change is made until the kill. The seeded commit is the only one, so that an agreement at any time is at it.
	By("waiting for Synced to say that the volume and the repository agree at the seeded commit")
	Eventually(func(g Gomega) {
		t.expectSyncedAtTip(g, time.Time{})
	}, dodReportBound).Should(Succeed())

	By("reading the pod and the epoch of the writer lock that its agent took")
	var before syncPodState
	var epochBefore uint64
	Eventually(func(g Gomega) {
		before = t.syncPod(g)
		g.Expect(before.ready).To(BeTrue())
		_, epochBefore = lockTaken(g, t)
	}).Should(Succeed())

	// The log of the agent that is killed is kept from before the kill: the kubelet may remove the dead container, and its log, before it
	// is asked for (followAgentLog).
	killedLog := followAgentLog(t)

	By("killing the agent from the node")
	killed := time.Now()
	output, err := utils.KillContainer(t.namespace, t.syncPodName(), failureAgentContainer)
	Expect(err).NotTo(HaveOccurred(), "Failed to kill the agent")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)

	By("writing a file in the pod of the application and pushing a commit from Git while the agent is down")
	t.appWrite("while-down-app.txt", "written while the agent was down")
	t.pushFromGit("while-down-git.txt", "pushed while the agent was down")
	wrote := time.Now()

	By("waiting for the kubelet to start the agent again in the same pod, and for the pod to be ready")
	var after syncPodState
	Eventually(func(g Gomega) {
		after = t.syncPod(g)
		g.Expect(after.uid).To(Equal(before.uid), "the pod was replaced, and not its container started again")
		g.Expect(after.restarts).To(Equal(before.restarts+1), "the container was not started again, or more than once")
		g.Expect(after.ready).To(BeTrue())
	}, failureKillBound).Should(Succeed())
	ready := time.Now()
	exitCode, err := utils.Run(exec.Command("kubectl", "get", "pod", t.syncPodName(), "-n", t.namespace,
		"-o", "jsonpath={.status.containerStatuses[0].lastState.terminated.exitCode}"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the sync")
	Expect(exitCode).To(Equal("137"), "the container that was killed did not end with SIGKILL")

	By("checking in its log that the new agent waited a TTL for the lock, took it with a higher epoch and reopened the checkout")
	var took time.Time
	var epochAfter uint64
	Eventually(func(g Gomega) {
		took, epochAfter = lockTaken(g, t)
	}).Should(Succeed())
	_, epochKilled := lockTakenIn(Default, killedLog.logOfEndedContainer(Default))
	Expect(epochKilled).To(Equal(epochBefore), "the log of the container that was killed is not the one of the agent that was read before the kill")
	Expect(epochAfter).To(BeNumerically(">", epochBefore), "the new agent took the lock with an epoch that is not higher")
	// The new agent takes the lock after it has seen the slot of the killed one for a TTL from its first look, which is after the
	// container started. The start is in whole seconds, rounded down, and both times are of the clock of the node.
	Expect(took.Sub(after.started)).To(BeNumerically(">=", failureLockTTL),
		"the new agent took the lock %s after its container started, under the TTL", took.Sub(after.started))
	// The changes were made while the new agent waited for the lock, so while no agent synced. The node of kind is a container on the
	// machine that runs the suite, with its clock.
	Expect(took).To(BeTemporally(">", wrote), "the new agent held the lock before the changes were made")
	logs, err := utils.Run(exec.Command("kubectl", "logs", t.syncPodName(), "-n", t.namespace))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the log of the agent")
	Expect(logs).To(ContainSubstring("reopened the checkout of the branch "+t.branch), "the new agent made another checkout")

	By("seeing the file of the application on the remote and the file from Git in the pod of the application")
	Eventually(func(g Gomega) {
		g.Expect(t.remoteFiles(g)).To(ContainElement("while-down-app.txt"))
		g.Expect(t.appFile(g, "while-down-git.txt")).To(Equal("pushed while the agent was down"))
	}, dodBound).Should(Succeed())

	By("seeing Synced say that the volume and the repository agree at the tip of the remote")
	Eventually(func(g Gomega) {
		t.expectSyncedAtTip(g, killed)
	}, dodReportBound).Should(Succeed())
	synced := time.Now()
	_, _ = fmt.Fprintf(GinkgoWriter, "after the kill: the lock was taken at %s from epoch %d to %d, ready at %s, synced at %s\n",
		took.Sub(killed).Round(time.Second), epochBefore, epochAfter, ready.Sub(killed).Round(time.Second),
		synced.Sub(killed).Round(time.Second))

	By("checking that the container was started again once and no more")
	state := t.syncPod(Default)
	Expect(state.uid).To(Equal(before.uid), "the pod was replaced")
	Expect(state.restarts).To(Equal(before.restarts+1), "the container was started again more than once")
	Expect(state.ready).To(BeTrue(), "the pod is not ready")
}

// lockTaken reads the log of the agent of the container that runs, and returns when it took the writer lock and its epoch, by the
// clock of the node. The log of a container that has ended is not read from the pod: followAgentLog keeps it.
func lockTaken(g Gomega, t syncTarget) (time.Time, uint64) {
	logs, err := utils.Run(exec.Command("kubectl", "logs", t.syncPodName(), "-n", t.namespace))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the log of the agent")
	return lockTakenIn(g, logs)
}

// lockTakenIn returns when the agent that wrote the log took the writer lock and its epoch, by the clock of the node.
func lockTakenIn(g Gomega, logs string) (time.Time, uint64) {
	match := lockLine.FindStringSubmatch(logs)
	g.Expect(match).NotTo(BeNil(), "the agent has not said that it holds the writer lock")
	took, err := time.Parse(time.RFC3339, match[1])
	g.Expect(err).NotTo(HaveOccurred(), "the time of the line is %q", match[1])
	epoch, err := strconv.ParseUint(match[2], 10, 64)
	g.Expect(err).NotTo(HaveOccurred(), "the epoch is %q", match[2])
	return took, epoch
}

// setUpFailure seeds the repository of a failure spec on the remote, from a clone of its own that the pushes from Git use, and
// applies its claim, its Treebind and the pod of its application.
func setUpFailure(t syncTarget) {
	By("seeding a repository of its own on the remote")
	_, err := remoteShell("git init -q --bare --initial-branch=" + t.branch + " " + t.repository + " && git init -q --initial-branch=" +
		t.branch + " " + t.clone + " && cd " + t.clone + " && git config user.name dev && git config user.email dev@example.com && " +
		"echo hello > " + failureSeedFile + " && git add -A && git commit -q -m first && git push -q " + t.repository + " " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to seed the repository")

	By("applying its claim, its Treebind and the pod of its application")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(failureManifest(t))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the manifest")
}

// tearDownFailure removes what setUpFailure made, so that a spec that failed leaves nothing to the next: the Treebind, whose pod of
// the sync goes with it, the pod of the application, the claim, the repository and the clone.
func tearDownFailure(t syncTarget) {
	By("removing the Treebind, the pod of the application, the claim and the repository")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", t.namespace, "treebind/"+t.name, "pod/"+t.appPod,
		"--ignore-not-found", "--grace-period=0", "--force"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", t.namespace, "pvc/"+t.name+"-data", "--ignore-not-found",
		"--timeout=3m"))
	_, _ = remoteShell("rm -rf " + t.repository + " " + t.clone)
}

// diagnoseFailure writes to the log of the spec, when it failed, what tells why: the logs of the agent, also of the container that
// was killed, how the Treebind and the pod are described, the events and the commits of the remote.
func diagnoseFailure(t syncTarget) {
	if !CurrentSpecReport().Failed() {
		return
	}
	for _, args := range [][]string{
		{"logs", t.syncPodName(), "-n", t.namespace},
		{"logs", t.syncPodName(), "-n", t.namespace, "--previous"},
		{"describe", "treebind", t.name, "-n", t.namespace},
		{"describe", "pod", t.syncPodName(), "-n", t.namespace},
		{"get", "events", "-n", t.namespace, "--sort-by=.lastTimestamp"},
	} {
		output, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(%v)\n", strings.Join(args, " "), output, err)
	}
	output, err := remoteShell("git -C " + t.repository + " log --format='%h %an %s' -10 " + t.branch)
	_, _ = fmt.Fprintf(GinkgoWriter, "the remote:\n%s\n(%v)\n", output, err)
}
