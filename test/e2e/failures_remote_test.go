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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mohanedmoh/treebind/test/utils"
)

// The remote of hack/dev-ssh.sh cannot be stopped as a pod: its repositories are in an emptyDir and its host keys are made when
// its container starts, so a remote that was deleted or scaled to zero comes back empty, with a host key that the known hosts
// of the Treebinds do not have. What goes away is its Service: its selector is set to one that matches no pod, and is put back
// as it was. kube-proxy rejects a connection to a Service that has no endpoints, which ssh reports as Connection refused. On
// kind, in its iptables mode, each try was refused a second after it began, when the SYN was sent again, and none waited for a
// timeout, which would hold the loop of the agent for minutes (#55). The pod, its repositories and its key stay, and
// remoteShell, which reaches the pod with kubectl exec through the Deployment and not through the Service, still pushes from
// Git while the agent cannot reach it.
const (
	// remoteService is the Service of the remote, by which a Treebind reaches it.
	remoteService = remoteDeployment
	// remoteAwaySelector is the selector of the Service while the remote is away: a label that no pod has.
	remoteAwaySelector = `{"treebind.example.org/e2e":"remote-away"}`

	awayAppFile    = "while-away-app.txt"
	awayAppContent = "written while the remote was away"
	awayGitFile    = "while-away-git.txt"
	awayGitContent = "pushed while the remote was away"

	// failureOutageBound is how long the agent and the operator have, from the moment the remote is taken away, to say so, and so
	// how long it stays away at most. The next pull of the agent is due within an interval of the Treebind and is refused within a
	// second, and the operator asks the agent every 30 seconds, a fifth more or less, so Synced says FetchFailed one poll later: on
	// kind 27 to 32 seconds after the remote was taken away.
	failureOutageBound = time.Minute
	// failureRecoveryBound is how long the agent has, from the moment the remote is back, to bring both changes to the other side.
	// After a pull that failed the next one waits a random time between the interval and the interval doubled once for each pull
	// in a row that failed, up to ten minutes (syncloop.Backoff), so the length of the outage sets it. A simulation of the
	// back-off for the 5 seconds of the Treebind puts the next pull within 3 minutes 99.997% of the time after an outage of 45
	// seconds, about the longest that one poll of the operator makes it, and within 4.5 minutes 99.97% of the time after a minute,
	// the most that failureOutageBound lets it last. The push of what that pull resolves comes at the scan that the files it
	// writes call for. On kind two or three pulls failed while the remote was away, and the files were on both sides 2 to 26
	// seconds after it was back.
	failureRecoveryBound = 5 * time.Minute
)

// The lines of the log of the agent that say that a pull and a push failed because the remote could not be reached. The agent
// logs a check when what it says changes, so the first failure of each is one line and the ones that follow it are none.
var (
	pullFailedLine = regexp.MustCompile(`(?m)^time=\S+ level=WARN msg=pull check=Pull outcome=Failed reason=FetchFailed .*$`)
	pushFailedLine = regexp.MustCompile(`(?m)^time=\S+ level=WARN msg=push check=Push outcome=Failed reason=PushFailed .*$`)
	// acceptedLine is the line of the log of sshd that says that it let the agent in, with the time that kubectl logs puts before
	// it.
	acceptedLine = regexp.MustCompile(`(?m)^(\S+) Accepted publickey for git from`)
)

// remoteDown is the Context of the remote that goes away and comes back, which failureInjection calls.
func remoteDown() {
	target := failureTarget("remote-down")
	BeforeAll(func() { setUpFailure(target) })
	AfterAll(func() { tearDownFailure(target) })
	// The remote is shared with the specs that come after, so it is put back whether the spec passed or not, once a spec that
	// failed has said why. It is put back here and not by a DeferCleanup of the spec: Ginkgo runs that after the AfterAll nodes,
	// and the one of the failure specs removes the remote when this Context is the last.
	AfterEach(func() {
		diagnoseFailure(target)
		diagnoseRemoteService()
		bringRemoteBack()
	})

	It("should say FetchFailed while the remote is away and bring each change to the other side once when it is back", func() {
		remoteAwayAndBack(target)
	})
}

// remoteAwayAndBack takes the Service of the remote away from a Treebind that agrees with it, while the application writes a
// file and a commit is pushed from Git, and checks that the Treebind says why with Ready kept True, and that once the Service
// is back both changes arrive on the other side within the bound, each added once, and the status says that the two sides agree
// at the tip.
func remoteAwayAndBack(t syncTarget) {
	By("waiting for Synced to say that the volume and the repository agree at the seeded commit")
	var seeded string
	Eventually(func(g Gomega) {
		g.Expect(t.appFile(g, failureSeedFile)).To(Equal("hello"))
		seeded = t.expectSyncedAtTip(g, time.Time{})
	}, dodRestartBound).Should(Succeed())
	before := t.syncPod(Default)

	By("taking the Service of the remote away from its pod")
	away := time.Now()
	takeRemoteAway()

	By("writing a file in the pod of the application, and pushing a commit from Git in the pod of the remote")
	t.appWrite(awayAppFile, awayAppContent)
	t.pushFromGit(awayGitFile, awayGitContent)

	// Both the pull and the push wait for the repository, so Ready stays True (docs/adr/0037). Synced takes the reason of the
	// first step of the report that failed, and the pull comes before the push in it, so it says FetchFailed from the first pull
	// that failed until a pull passes, whatever the push says. Only in the seconds between the push of the commit of the
	// application and the next pull can a poll find the push alone failed, and Synced say PushFailed.
	By("seeing Synced say FetchFailed at the seeded commit with Ready True, and the agent fail to push the application's commit")
	var seen []string
	Eventually(func(g Gomega) {
		ready, synced, reason, message, agreed := t.syncedWithMessage(g)
		if reading := synced + "/" + reason; len(seen) == 0 || seen[len(seen)-1] != reading {
			seen = append(seen, reading)
			_, _ = fmt.Fprintf(GinkgoWriter, "%s after the remote was taken away: Synced is %s\n", since(away), reading)
		}
		g.Expect(synced).To(Equal("False"), "Synced is %q with the reason %q", synced, reason)
		g.Expect(reason).To(Equal("FetchFailed"))
		g.Expect(message).To(HavePrefix("The last pull failed: the repository could not be asked"))
		g.Expect(message).To(ContainSubstring("The sync agent tries again by itself"))
		g.Expect(message).To(ContainSubstring("last agreed at commit " + seeded[:12]))
		g.Expect(agreed).To(Equal(seeded), "status.lastSyncedCommit moved while the remote was away")
		g.Expect(ready).To(Equal("True"), "Ready is %q while Synced waits for the repository", ready)

		logs := t.agentLogSince(g, away)
		g.Expect(pullFailedLine.FindString(logs)).To(ContainSubstring("Connection refused"),
			"the agent has not said that a pull was refused")
		g.Expect(pushFailedLine.MatchString(logs)).To(BeTrue(), "the agent has not said that a push failed")
	}, time.Until(away.Add(failureOutageBound))).Should(Succeed())
	outage := time.Since(away)

	By("bringing the Service of the remote back")
	back := time.Now()
	bringRemoteBack()

	By("seeing both files on both sides, and no other")
	Eventually(func(g Gomega) {
		g.Expect(t.remoteFiles(g)).To(ConsistOf(failureSeedFile, awayAppFile, awayGitFile))
		g.Expect(t.appFile(g, awayGitFile)).To(Equal(awayGitContent))
	}, failureRecoveryBound).Should(Succeed())
	arrived := time.Now()
	Expect(t.appFiles()).To(ConsistOf(failureSeedFile, awayAppFile, awayGitFile))
	Expect(t.remoteFile(awayAppFile)).To(Equal(awayAppContent))

	By("checking that one commit of the remote adds each file")
	for _, name := range []string{awayAppFile, awayGitFile} {
		Expect(t.remoteCommitsAdding(name)).To(HaveLen(1), "the history of the remote adds %s more than once, or never", name)
	}

	By("seeing Synced say that the volume and the repository agree at the tip of the remote, with no conflict waiting")
	var tip string
	Eventually(func(g Gomega) {
		tip = t.expectSyncedAtTip(g, back)
	}, dodReportBound).Should(Succeed())
	synced := time.Now()
	Expect(tip).NotTo(Equal(seeded))
	_, _ = fmt.Fprintf(GinkgoWriter, "the remote was away for %s; once it was back the agent asked it again after %s, both files "+
		"were on both sides after %s and Synced was True after %s\n", outage.Round(time.Second), firstAcceptedAfter(back),
		arrived.Sub(back).Round(time.Second), synced.Sub(back).Round(time.Second))

	By("checking that the agent was not started again, and that it resolved the two commits")
	after := t.syncPod(Default)
	Expect(after.uid).To(Equal(before.uid), "the pod of the sync was replaced")
	Expect(after.restarts).To(Equal(before.restarts), "the agent was started again")
	// The volume had a commit that the remote lacked and the remote one that the volume lacked, so the first pull that passed
	// resolved the two by the policy of the Treebind, followRemote, which makes the commit of the file of the application again on
	// the tip of the remote (docs/adr/0034). The Conflict check keeps the last resolution while the agent runs.
	Expect(t.sync(Default).conflictReason).To(Equal("ConflictResolved"), "the two sides were brought together without a resolution")
}

// remoteSelectorBefore is the selector that the Service of the remote had before takeRemoteAway changed it, and is empty while
// the Service has it.
var remoteSelectorBefore string

// takeRemoteAway sets the selector of the Service of the remote to one that matches no pod, and waits for the Service to have
// no endpoint. It keeps the selector before it changes it, so that bringRemoteBack puts it back whatever fails after.
func takeRemoteAway() {
	selector := remoteSelector(Default)
	Expect(selector).NotTo(Equal(remoteAwaySelector), "the remote was left away")
	remoteSelectorBefore = selector
	setRemoteSelector(remoteAwaySelector)
	Eventually(func(g Gomega) {
		g.Expect(remoteEndpoints(g)).To(BeEmpty(), "the Service of the remote still has endpoints")
	}).Should(Succeed())
}

// bringRemoteBack puts back the selector that takeRemoteAway changed, and waits for the Service to have the pod of the remote as
// an endpoint that is ready again. It does nothing when the Service has its selector.
func bringRemoteBack() {
	selector := remoteSelectorBefore
	if selector == "" {
		return
	}
	remoteSelectorBefore = ""
	setRemoteSelector(selector)
	podIP, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", remoteNamespace, "-l",
		"app.kubernetes.io/component="+remoteDeployment, "-o", "jsonpath={.items[*].status.podIP}"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the remote")
	Eventually(func(g Gomega) {
		g.Expect(remoteSelector(g)).To(Equal(selector), "the selector of the Service is not the one it had")
		g.Expect(remoteEndpoints(g)).To(ConsistOf(strings.TrimSpace(podIP) + "=true"))
	}).Should(Succeed())
}

// remoteSelector is the selector of the Service of the remote, as the JSON that kubectl prints of it.
func remoteSelector(g Gomega) string {
	output, err := utils.Run(exec.Command("kubectl", "get", "service", remoteService, "-n", remoteNamespace,
		"-o", "jsonpath={.spec.selector}"))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the Service of the remote")
	return strings.TrimSpace(output)
}

// setRemoteSelector replaces the whole selector of the Service of the remote, which a merge patch would add to instead.
func setRemoteSelector(selector string) {
	_, err := utils.Run(exec.Command("kubectl", "patch", "service", remoteService, "-n", remoteNamespace, "--type=json",
		"-p", `[{"op":"replace","path":"/spec/selector","value":`+selector+`}]`))
	Expect(err).NotTo(HaveOccurred(), "Failed to set the selector of the Service of the remote")
}

// remoteEndpoints lists the endpoints of the Service of the remote, each as its address and whether it is ready.
func remoteEndpoints(g Gomega) []string {
	output, err := utils.Run(exec.Command("kubectl", "get", "endpointslices", "-n", remoteNamespace,
		"-l", "kubernetes.io/service-name="+remoteService,
		"-o", `jsonpath={range .items[*].endpoints[*]}{.addresses[0]}={.conditions.ready}{"\n"}{end}`))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the endpoints of the Service of the remote")
	return utils.GetNonEmptyLines(output)
}

// syncedWithMessage reads the status of the Treebind once: Ready, Synced with its reason and message, and
// status.lastSyncedCommit.
func (t syncTarget) syncedWithMessage(g Gomega) (ready, synced, reason, message, agreed string) {
	condition := func(conditionType, field string) string {
		return `{.status.conditions[?(@.type=="` + conditionType + `")].` + field + `}`
	}
	output, err := utils.Run(exec.Command("kubectl", "get", "treebind", t.name, "-n", t.namespace, "-o", "jsonpath="+
		condition("Ready", "status")+"|"+condition("Synced", "status")+"|"+condition("Synced", "reason")+"|"+
		condition("Synced", "message")+"|{.status.lastSyncedCommit}"))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the Treebind")
	// No message of the operator has a bar in it.
	values := strings.Split(strings.TrimSpace(output), "|")
	g.Expect(values).To(HaveLen(5), "kubectl answered %q", output)
	return values[0], values[1], values[2], values[3], values[4]
}

// agentLogSince is the log of the agent of the Treebind from a time on, which kubectl takes in whole seconds.
func (t syncTarget) agentLogSince(g Gomega, from time.Time) string {
	output, err := utils.Run(exec.Command("kubectl", "logs", t.syncPodName(), "-n", t.namespace,
		"--since-time="+from.UTC().Truncate(time.Second).Format(time.RFC3339)))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the log of the agent")
	return output
}

// appFiles lists the directory that the pod of the application mounts, with the files whose names begin with a dot.
func (t syncTarget) appFiles() []string {
	output, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "ls", "-A", t.appMount))
	Expect(err).NotTo(HaveOccurred(), "Failed to list the files in the pod of the application")
	return utils.GetNonEmptyLines(output)
}

// remoteFile is the content of a file of the branch of the repository of the remote.
func (t syncTarget) remoteFile(name string) string {
	output, err := remoteShell("git -C " + t.repository + " show " + shellQuote(t.branch+":"+name))
	Expect(err).NotTo(HaveOccurred(), "Failed to read a file of the repository")
	return output
}

// remoteCommitsAdding lists the commits of the branch of the repository of the remote that add a file, in every line of its
// history: a file that two commits add came to the remote twice.
func (t syncTarget) remoteCommitsAdding(name string) []string {
	output, err := remoteShell("git -C " + t.repository + " log --full-history --diff-filter=A --format=%h " + t.branch +
		" -- " + shellQuote(name))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the history of the repository")
	return utils.GetNonEmptyLines(output)
}

// firstAcceptedAfter says how long after a time sshd of the remote first let a client in, which in a failure spec is its agent:
// it is when the agent asked the remote again after it came back. It is for the log of the spec, and says so when it cannot
// tell.
func firstAcceptedAfter(from time.Time) string {
	output, err := utils.Run(exec.Command("kubectl", "logs", "deploy/"+remoteDeployment, "-n", remoteNamespace, "--timestamps",
		"--since-time="+from.UTC().Truncate(time.Second).Format(time.RFC3339)))
	match := acceptedLine.FindStringSubmatch(output)
	if err != nil || match == nil {
		return "a time that the log of the remote does not say"
	}
	accepted, err := time.Parse(time.RFC3339Nano, match[1])
	if err != nil {
		return "a time that the log of the remote does not say"
	}
	return accepted.Sub(from).Round(time.Second).String()
}

// since is the time from a moment until now, in whole seconds.
func since(from time.Time) string { return time.Since(from).Round(time.Second).String() }

// diagnoseRemoteService writes to the log of the spec, when it failed, the Service of the remote, its endpoints and the last
// lines of the log of sshd. It runs before the selector is put back.
func diagnoseRemoteService() {
	if !CurrentSpecReport().Failed() {
		return
	}
	for _, args := range [][]string{
		{"get", "service", remoteService, "-n", remoteNamespace, "-o", "yaml"},
		{"get", "endpointslices", "-n", remoteNamespace, "-l", "kubernetes.io/service-name=" + remoteService, "-o", "yaml"},
		{"logs", "deploy/" + remoteDeployment, "-n", remoteNamespace, "--timestamps", "--tail=40"},
	} {
		output, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(%v)\n", strings.Join(args, " "), output, err)
	}
}
