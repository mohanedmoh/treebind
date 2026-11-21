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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mohanedmoh/treebind/test/utils"
)

// The definition of done of the MVP: a new user applies one manifest, edits a file in the app pod and sees a commit on the remote
// within the configured interval. They then push a change from Git, see it appear in the pod, and read the outcomes in kubectl
// describe: the conditions Synced and Conflict and the commit that the volume and the repository last agreed on (docs/adr/0037).
// It runs with an agent that is the real one, against the ssh remote that hack/dev-ssh.sh starts, which is the one that
// docs/development.md tells a contributor to use, and the pod that edits mounts the claim with the same subPath as an application
// does (docs/git-directory.md).
const (
	dodNamespace  = "treebind-dod-e2e"
	dodName       = "dod"
	dodClaim      = dodName + "-data"
	dodSubPath    = "dashboards"
	dodAppPod     = dodName + "-app"
	dodSyncPod    = dodName + "-sync-0"
	dodAppMount   = "/data"
	dodAppImage   = "busybox"
	dodBranch     = "main"
	dodAuthor     = "treebind-bot"
	dodMessage    = "treebind: sync 1 file(s)"
	dodSeedFile   = "hello.txt"
	dodInterval   = "5s"
	dodSettleTime = "1s"

	// remoteNamespace and remoteDeployment are the ssh remote of hack/dev-ssh.sh, and its repository is bare in remoteRepository.
	remoteNamespace  = "treebind-dev"
	remoteDeployment = "git-remote-ssh"
	remoteRepository = "/srv/sample.git"
	remoteURL        = "ssh://git@git-remote-ssh.treebind-dev.svc.cluster.local" + remoteRepository

	// dodBound is how long a change has to arrive in the other place: an interval for the pull or the scan, the time a file has to be
	// quiet, and what a cluster that is not quick adds. The specs wait for the change and not for this long.
	dodBound = 60 * time.Second
	// dodRestartBound is how long a pod that was deleted has to be back, ready and following the repository again.
	dodRestartBound = 3 * time.Minute
	// dodReportBound is how long the operator has, after a change has arrived in the other place, to write what the agent reports of it
	// in the status of the Treebind. It asks the agent every 30 seconds, a fifth more or less, and every minute after a poll that
	// failed, and the poll that comes first after a spec or a new leader may be one of those.
	dodReportBound = 2 * time.Minute

	// rfc3339 matches a time as the API server writes it in the status, which kubectl describe prints as it is.
	rfc3339 = `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`
)

// dodManifest is the claim, the Treebind and the pod of the application. The key and the host keys are the Secret and the ConfigMap
// that hack/dev-ssh.sh makes in the namespace.
const dodManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ` + dodClaim + `
  namespace: ` + dodNamespace + `
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 100Mi
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: ` + dodName + `
  namespace: ` + dodNamespace + `
spec:
  repository:
    url: ` + remoteURL + `
    branch: ` + dodBranch + `
    auth:
      sshKeySecretRef: {name: dev-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: dev-known-hosts, key: known_hosts}
  target:
    claimName: ` + dodClaim + `
    subPath: ` + dodSubPath + `
  sync:
    direction: bidirectional
    interval: ` + dodInterval + `
    settleTime: ` + dodSettleTime + `
---
apiVersion: v1
kind: Pod
metadata:
  name: ` + dodAppPod + `
  namespace: ` + dodNamespace + `
spec:
  containers:
  - name: app
    image: ` + dodAppImage + `
    command: ["sleep", "3600"]
    volumeMounts:
    - {name: data, mountPath: ` + dodAppMount + `, subPath: ` + dodSubPath + `}
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: ` + dodClaim + `
`

// seedScript puts the first commit in the repository of the remote, from a clone that the later pushes use as well.
const seedScript = `git init -q --initial-branch=` + dodBranch + ` /tmp/seed && cd /tmp/seed &&
git config user.name dev && git config user.email dev@example.com &&
echo hello > ` + dodSeedFile + ` && git add -A && git commit -q -m first && git push -q ` + remoteRepository + ` ` + dodBranch

// agentImage is the sync agent image that ko built into the cluster for these specs, which are the ones that run an agent. It is
// built when they begin, by installAgentImage, so that a run that is focused on other specs needs neither ko nor the network for it.
var agentImage string

// definitionOfDone is the Context of the specs below. It is called from the Describe of the manager, whose BeforeAll deploys the
// operator, and it comes last: it gives the operator the agent image, which replaces its pods.
func definitionOfDone() {
	BeforeAll(func() {
		installAgentImage()
		startRemoteFor(dodNamespace)

		By("seeding the repository of the remote")
		_, err := remoteShell(seedScript)
		Expect(err).NotTo(HaveOccurred(), "Failed to seed the repository")

		// The pod of the application is in the manifest with the Treebind, as a user's would be, and may start before the pod of the
		// sync: the kubelet then makes the directory of the subPath, which it makes world-writable (tried: the agent clones into it).
		By("applying the one manifest: the claim, the Treebind and the pod of the application")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(dodManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the manifest")
	})

	AfterAll(func() {
		By("removing the namespace of the Treebind and the remote")
		for _, ns := range []string{dodNamespace, remoteNamespace} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", ns, "--ignore-not-found", "--timeout=3m"))
		}
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		for _, args := range [][]string{
			{"logs", dodSyncPod, "-n", dodNamespace},
			{"describe", "treebind", dodName, "-n", dodNamespace},
			{"describe", "pod", dodSyncPod, "-n", dodNamespace},
			{"get", "events", "-n", dodNamespace, "--sort-by=.lastTimestamp"},
		} {
			output, err := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(%v)\n", strings.Join(args, " "), output, err)
		}
		output, err := remoteShell("git -C " + remoteRepository + " log --format='%h %an %s' -10 " + dodBranch)
		_, _ = fmt.Fprintf(GinkgoWriter, "the remote:\n%s\n(%v)\n", output, err)
	})

	It("should be ready, with the files of the repository in the pod of the application", func() {
		By("waiting for the Treebind to be ready")
		Eventually(treebindCondition, dodRestartBound).Should(Equal("True"))

		By("seeing the seeded file in the pod of the application")
		Eventually(func(g Gomega) {
			g.Expect(appFile(g, dodSeedFile)).To(Equal("hello"))
		}, dodBound).Should(Succeed())
	})

	It("should commit what the application writes, within the interval", func() {
		By("writing a file in the pod of the application")
		appWrite("from-the-app.txt", "written by the application")

		By("seeing the commit on the remote, by the author and with the message of the Treebind")
		Eventually(func(g Gomega) {
			g.Expect(remoteFiles(g)).To(ContainElement("from-the-app.txt"))
			g.Expect(remoteLastCommit(g)).To(Equal(dodAuthor + "|" + dodMessage))
		}, dodBound).Should(Succeed())
		Expect(remoteFile("from-the-app.txt")).To(Equal("written by the application"))
	})

	It("should bring in what is pushed to the remote, within the interval", func() {
		By("pushing a commit from Git")
		pushFromGit("from-git.txt", "pushed from git")

		By("seeing the file in the pod of the application")
		Eventually(func(g Gomega) {
			g.Expect(appFile(g, "from-git.txt")).To(Equal("pushed from git"))
		}, dodBound).Should(Succeed())
	})

	It("should keep both directions after the sync pod is deleted with its grace period", func() {
		restartAndCheck("graceful", "--wait=true")
	})

	It("should keep both directions after the sync pod is deleted with no grace period", func() {
		restartAndCheck("forced", "--grace-period=0", "--force")
	})

	It("should keep both directions after the leader of the operator is deleted", func() {
		var leader string
		Eventually(func(g Gomega) {
			leader = leaseHolderPod(g)
			g.Expect(leader).NotTo(BeEmpty())
		}).Should(Succeed())

		By("deleting the leader of the operator without a grace period")
		_, err := utils.Run(exec.Command("kubectl", "delete", "pod", leader, "-n", namespace, "--grace-period=0", "--force"))
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the leader pod")

		By("checking that the sync goes on in both directions while the operator is replaced")
		bothDirections("after-the-leader")

		By("waiting for the operator to be whole again")
		Eventually(func(g Gomega) {
			g.Expect(readyControllerPods(g)).To(HaveLen(controllerReplicas))
		}, dodRestartBound).Should(Succeed())
	})

	It("should say in kubectl describe that the Treebind is ready and that its agent can be read", func() {
		Eventually(func(g Gomega) {
			g.Expect(treebindConditionOf(g, "Ready")).To(Equal("True"))
			g.Expect(treebindConditionOf(g, "AgentUnreachable")).To(Equal("False"))
		}, dodRestartBound).Should(Succeed())

		output, err := utils.Run(exec.Command("kubectl", "describe", "treebind", dodName, "-n", dodNamespace))
		Expect(err).NotTo(HaveOccurred())
		for _, conditionType := range []string{"Ready", "AgentUnreachable", "Synced", "Conflict"} {
			// The padding between the key and the value is the one of kubectl's table, and is not what is looked for.
			Expect(output).To(MatchRegexp(`Type:\s+`+conditionType+`\n`), "kubectl describe does not show the condition %s", conditionType)
		}
	})

	// The operator writes the outcome of the sync from the report of the agent (docs/adr/0037), which it reads every 30 seconds, so the
	// status follows a change a poll after the agent has made it. Each wait for the status begins after the change has arrived in the
	// other place, and compares the commit that the status names with the tip of the remote, so that the status of the change before,
	// which names the tip before, is not taken for this one.
	It("should say in Synced, in kubectl describe, what the last commit and the last pull were", func() {
		By("writing a file in the pod of the application and seeing the commit on the remote")
		pushedAfter := time.Now()
		appWrite("synced-app.txt", "written by the application")
		Eventually(func(g Gomega) {
			g.Expect(remoteFiles(g)).To(ContainElement("synced-app.txt"))
		}, dodBound).Should(Succeed())

		By("seeing Synced say that the volume and the repository agree at the commit that the agent pushed")
		var pushed string
		Eventually(func(g Gomega) {
			pushed = expectSyncedAtTip(g, pushedAfter)
		}, dodReportBound).Should(Succeed())

		By("pushing a commit from Git and seeing the file in the pod of the application")
		pulledAfter := time.Now()
		pushFromGit("synced-git.txt", "pushed from git")
		Eventually(func(g Gomega) {
			g.Expect(appFile(g, "synced-git.txt")).To(Equal("pushed from git"))
		}, dodBound).Should(Succeed())

		By("seeing Synced say that they agree at the commit that the agent pulled")
		var pulled string
		Eventually(func(g Gomega) {
			pulled = expectSyncedAtTip(g, pulledAfter)
		}, dodReportBound).Should(Succeed())
		Expect(pulled).NotTo(Equal(pushed), "the commit pushed from Git is another than the one the agent pushed")

		By("reading the same in kubectl describe")
		Eventually(func(g Gomega) {
			output, err := utils.Run(exec.Command("kubectl", "describe", "treebind", dodName, "-n", dodNamespace))
			g.Expect(err).NotTo(HaveOccurred(), "Failed to describe the Treebind")
			// kubectl describe prints the fields of a condition in the order of their names, so that the reason, the status and the type
			// follow each other. The padding between a key and its value is the one of kubectl's table, and is not what is looked for.
			for _, condition := range []struct{ conditionType, status, reason string }{
				{"Ready", "True", "SyncPodReady"},
				{"Synced", "True", "InSync"},
				{"Conflict", "False", "(?:NoConflict|ConflictResolved)"},
			} {
				g.Expect(output).To(MatchRegexp(`Reason:\s+`+condition.reason+`\s+Status:\s+`+condition.status+`\s+Type:\s+`+
					condition.conditionType+`\n`), "kubectl describe does not show %s as %s with the reason %s",
					condition.conditionType, condition.status, condition.reason)
			}
			g.Expect(output).To(MatchRegexp(`Last Synced Commit:\s+`+pulled+`\n`), "kubectl describe does not show the commit")
			g.Expect(output).To(MatchRegexp(`Last Sync Time:\s+`+rfc3339+`\n`), "kubectl describe does not show the time")
		}).Should(Succeed())
	})
}

// restartAndCheck deletes the pod of the sync with the arguments, waits for the pod that replaces it to be ready and to have opened
// the checkout that was there, and checks that both directions go on.
func restartAndCheck(name string, deleteArgs ...string) {
	var before string
	Eventually(func(g Gomega) {
		state := syncPod(g)
		g.Expect(state.uid).NotTo(BeEmpty())
		before = state.uid
	}).Should(Succeed())

	By("deleting the pod of the sync (" + name + ")")
	args := append([]string{"delete", "pod", dodSyncPod, "-n", dodNamespace}, deleteArgs...)
	_, err := utils.Run(exec.Command("kubectl", args...))
	Expect(err).NotTo(HaveOccurred(), "Failed to delete the pod of the sync")

	By("waiting for another pod that is ready")
	Eventually(func(g Gomega) {
		// One read of one pod: the old pod, which is still there and ready while it ends, is not the new one, and a pod that is
		// not made yet is not a pod that differs from it.
		state := syncPod(g)
		g.Expect(state.uid).NotTo(BeEmpty(), "the pod is not made yet")
		g.Expect(state.uid).NotTo(Equal(before), "the pod is the one that was deleted")
		g.Expect(state.terminating).To(BeFalse())
		g.Expect(state.ready).To(BeTrue())
	}, dodRestartBound).Should(Succeed())

	By("checking that it opened the checkout that was on the volume and did not make another")
	Eventually(func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "logs", dodSyncPod, "-n", dodNamespace))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(ContainSubstring("reopened the checkout of the branch " + dodBranch))
	}, dodBound).Should(Succeed())

	By("checking that both directions go on")
	bothDirections("after-" + name)
}

// bothDirections writes a file in the pod of the application and sees it on the remote, and pushes a file from Git and sees it in the
// pod: a file is named by what comes before it, so that no file of an earlier spec can stand for it.
func bothDirections(prefix string) {
	appWrite(prefix+"-app.txt", "app "+prefix)
	Eventually(func(g Gomega) {
		g.Expect(remoteFiles(g)).To(ContainElement(prefix + "-app.txt"))
	}, dodBound).Should(Succeed())

	pushFromGit(prefix+"-git.txt", "git "+prefix)
	Eventually(func(g Gomega) {
		g.Expect(appFile(g, prefix+"-git.txt")).To(Equal("git " + prefix))
	}, dodBound).Should(Succeed())
}

// remoteShell runs a script as the user git in the pod of the ssh remote.
func remoteShell(script string) (string, error) {
	output, err := utils.Run(exec.Command("kubectl", "-n", remoteNamespace, "exec", "deploy/"+remoteDeployment, "--", "su", "git", "-c", script))
	return strings.TrimSpace(output), err
}

// remoteFiles lists the files of the branch of the repository of the remote.
func remoteFiles(g Gomega) []string {
	output, err := remoteShell("git -C " + remoteRepository + " ls-tree -r --name-only " + dodBranch)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to list the files of the repository")
	return utils.GetNonEmptyLines(output)
}

// remoteFile is the content of a file of the branch of the repository of the remote.
func remoteFile(name string) string {
	output, err := remoteShell("git -C " + remoteRepository + " show " + dodBranch + ":" + name)
	Expect(err).NotTo(HaveOccurred(), "Failed to read a file of the repository")
	return output
}

// remoteLastCommit is the author and the subject of the last commit of the branch, with a bar between them.
func remoteLastCommit(g Gomega) string {
	output, err := remoteShell("git -C " + remoteRepository + " log -1 --format='%an|%s' " + dodBranch)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the last commit of the repository")
	return output
}

// remoteTip is the full id of the tip of the branch of the repository of the remote.
func remoteTip(g Gomega) string {
	output, err := remoteShell("git -C " + remoteRepository + " rev-parse --verify refs/heads/" + dodBranch)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the tip of the repository")
	return output
}

// shellQuote makes a word of a shell of a string, whatever it holds: in single quotes, with each single quote closed, escaped and opened.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// pushFromGit makes a commit with a file in the clone that the seeding made, after taking what the repository has, and pushes it.
func pushFromGit(name, content string) {
	_, err := remoteShell("cd /tmp/seed && git pull -q " + remoteRepository + " " + dodBranch + " && echo " + shellQuote(content) + " > " +
		shellQuote(name) + " && git add -A && git commit -q -m " + shellQuote("from git: "+name) + " && git push -q " +
		remoteRepository + " " + dodBranch)
	Expect(err).NotTo(HaveOccurred(), "Failed to push from Git")
}

// appWrite writes a file in the directory of the claim that the pod of the application mounts, as the application does.
func appWrite(name, content string) {
	_, err := utils.Run(exec.Command("kubectl", "-n", dodNamespace, "exec", dodAppPod, "--", "sh", "-c",
		"echo "+shellQuote(content)+" > "+shellQuote(dodAppMount+"/"+name)))
	Expect(err).NotTo(HaveOccurred(), "Failed to write a file in the pod of the application")
}

// appFile is the content of a file in the pod of the application, without the newline at the end.
func appFile(g Gomega, name string) string {
	output, err := utils.Run(exec.Command("kubectl", "-n", dodNamespace, "exec", dodAppPod, "--", "cat", dodAppMount+"/"+name))
	g.Expect(err).NotTo(HaveOccurred(), "The file is not in the pod of the application yet")
	return strings.TrimSpace(output)
}

// treebindCondition is the status of the condition Ready of the Treebind.
func treebindCondition(g Gomega) string { return treebindConditionOf(g, "Ready") }

// treebindConditionOf is the status of a condition of the Treebind, or an empty string when it has none.
func treebindConditionOf(g Gomega, conditionType string) string {
	output, err := utils.Run(exec.Command("kubectl", "get", "treebind", dodName, "-n", dodNamespace,
		"-o", `jsonpath={.status.conditions[?(@.type=="`+conditionType+`")].status}`))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the Treebind")
	return strings.TrimSpace(output)
}

// treebindSyncState is what one read of the status of the Treebind says of the sync: the conditions that the operator writes from the
// report of the agent, with their reasons, and the two fields of the status. A condition or a field that is not there is empty.
type treebindSyncState struct {
	ready, synced, syncedReason, conflict, conflictReason, lastSyncedCommit, lastSyncTime string
}

// treebindSync reads the status of the Treebind once, so that what it says of the sync is of one moment.
func treebindSync(g Gomega) treebindSyncState {
	condition := func(conditionType, field string) string {
		return `{.status.conditions[?(@.type=="` + conditionType + `")].` + field + `}`
	}
	fields := []string{
		condition("Ready", "status"),
		condition("Synced", "status"), condition("Synced", "reason"),
		condition("Conflict", "status"), condition("Conflict", "reason"),
		"{.status.lastSyncedCommit}", "{.status.lastSyncTime}",
	}
	output, err := utils.Run(exec.Command("kubectl", "get", "treebind", dodName, "-n", dodNamespace,
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
func expectSyncedAtTip(g Gomega, notBefore time.Time) string {
	state := treebindSync(g)
	tip := remoteTip(g)
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
}

// syncPod reads the pod of the sync once, so that the uid, the readiness and the deletion are of the same pod.
func syncPod(g Gomega) syncPodState {
	output, err := utils.Run(exec.Command("kubectl", "get", "pod", dodSyncPod, "-n", dodNamespace, "--ignore-not-found",
		"-o", "jsonpath={.metadata.uid}|{.status.containerStatuses[0].ready}|{.metadata.deletionTimestamp}"))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the sync")
	uid, rest, _ := strings.Cut(strings.TrimSpace(output), "|")
	ready, deleted, _ := strings.Cut(rest, "|")
	return syncPodState{uid: uid, ready: ready == "true", terminating: deleted != ""}
}
