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
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mohanedmoh/treebind/test/utils"
)

// R-7: two agents write one checkout. Nothing refuses a second Treebind on a claim, and both keep their state in .treebind at the
// root of the claim, so the second one's agent waits for the writer lock for as long as the first one's holds it (docs/adr/0012,
// docs/adr/0023). The spec puts two Treebinds on one claim and freezes the agent that holds the lock for longer than the TTL, with
// the freezer of cgroup v2 from the node, as a stopped node or a partition does. The other agent takes the lock over, and when
// the first one is thawed its next check of the lock finds that it is lost.
//
// Nothing is changing on either side when the freeze begins, so what the frozen agent can be in the middle of is the lock's own
// slots and no Git runs: a holder that is frozen in the middle of a Git command finishes it after another agent took over, which
// ADR 0012 and ADR 0028 give as the limit of the lock, and which compare-and-swap branch updates (#106) are for. The spec does not
// test it.
const (
	// holderStandby is the second Treebind, on the claim and the subPath of the first.
	holderStandby = "holder-standby"
	// holderPeek is a pod that mounts the whole claim read-only, as root, to look at what the agents leave there. The pod of the
	// application mounts the subPath, and the agents keep the writer lock and the Git directory beside it, in .treebind at the root
	// of the claim, so that nothing else sees them.
	holderPeek  = "holder-peek"
	peekMount   = "/vol"
	peekLockDir = peekMount + "/.treebind/lock"

	frozenAppFile   = "while-frozen-app.txt"
	frozenAppText   = "written while the holder was frozen"
	releasedAppFile = "after-release-app.txt"
	releasedAppText = "written after the second agent gave the lock up"
	releasedGitFile = "after-release-git.txt"
	releasedGitText = "pushed after the second agent gave the lock up"

	// lockInterval is how often the holder makes a slot and a contender looks: a third of the TTL (internal/agent/lock).
	lockInterval = failureLockTTL / 3
	// holderWaitFor is how long the second agent is watched while it waits for a holder that runs: longer than the TTL and a look
	// after it, so that it would have taken the lock if the heartbeat did not keep it.
	holderWaitFor = failureLockTTL + lockInterval
	// holderTakeoverBound is how long the second agent has, from the freeze, to take the lock. It has seen the last slot of the
	// holder for the first time within a look of it (10 seconds after it was made at most), and takes the lock at the look that
	// comes a TTL after that, so within 40 seconds of the freeze, and on kind within 24 to 30. The kubelet kills a frozen container
	// no earlier than 52 seconds after the freeze (below), and the spec has to thaw it before, which a bound of 50 seconds holds to.
	holderTakeoverBound = failureLockTTL + 2*lockInterval
	// holderReleaseBound is how long the first agent has, from the deletion of the second Treebind, to take the lock: the pod of
	// the second is stopped, its agent gives the lock up when it stops, and the first sees the release at its next look.
	holderReleaseBound = 2 * time.Minute
)

// The liveness probe of the pod of the sync asks every 10 seconds, waits 2 and fails after 3 misses in a row (internal/controller).
// On kind the probe of a frozen agent failed 9 to 12, 19 to 22 and 29 to 32 seconds after the freeze, and at the third the kubelet
// sent the container SIGTERM, which a frozen process does not handle. It sent SIGKILL 30 seconds later, the grace period of the pod,
// about a minute after the freeze, and a frozen container does die of that, with the code 137. The second agent took the lock 24 to
// 30 seconds after the freeze. The two are within seconds of each other, and which comes first depends on where the freeze falls
// among the probes of the kubelet and the looks of the second agent, which the spec does not control. So the spec thaws the first
// agent as soon as it sees the takeover, which is always well before the SIGKILL, and says which way it went:
//   - The probe had not failed three times yet. The agent wakes with nothing pending, its keeper finds the lock lost at once and it
//     ends with the code 3 (docs/adr/0023).
//   - The kubelet had sent SIGTERM already. The agent wakes with it pending, and either finds the lock lost, as above, or stops on
//     request with the code 0. Then its release finds a higher slot, gives nothing up and logs that, or its keeper finds the loss
//     after Run has stopped listening for it, and nothing says that the lock was lost: the log has only that it stopped.
//
// Either way the container is started again once, and what the agent must not do is the same.
const (
	// exitLockLost is the exit code of an agent that lost the writer lock (cmd/agent), and exitStopped that of one that was told to
	// stop and did.
	exitLockLost = "3"
	exitStopped  = "0"
)

// lockSlot is a slot of the writer lock as the volume holds it: the number in the name of the file and what the holder wrote in it
// (internal/agent/lock).
type lockSlot struct {
	number   uint64
	Epoch    uint64    `json:"epoch"`
	Holder   string    `json:"holder"`
	Released bool      `json:"released"`
	Time     time.Time `json:"time"`
}

// heldBy says whether the pod of the sync t made the slot: a holder is the name of the pod and a token of each start.
func (s lockSlot) heldBy(t syncTarget) bool { return strings.HasPrefix(s.Holder, t.syncPodName()+"-") }

// frozenHolder is the Context of the two Treebinds on one claim, which failureInjection calls.
func frozenHolder() {
	first := failureTarget("holder")
	second := first
	second.name = holderStandby

	BeforeAll(func() {
		setUpFailure(first)
		By("applying the pod that looks at the claim")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(holderPeekManifest(first))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the pod that looks at the claim")
	})
	AfterAll(func() { tearDownHolder(first, second) })
	// Whatever fails, the container is thawed: a frozen one that was left would be killed by the kubelet a minute on, and the Treebind
	// that is removed after it waits for the grace period of a pod whose process does not stop.
	AfterEach(func() {
		output, err := utils.ThawContainer(first.namespace, first.syncPodName(), failureAgentContainer)
		_, _ = fmt.Fprintf(GinkgoWriter, "thawing the agent of the first Treebind: %s (%v)\n", output, err)
		diagnoseHolder(first, second)
	})

	It("should hand the lock to the second agent while the holder is frozen, stop the holder when it wakes and sync again "+
		"once the second is gone", func() {
		frozenHolderTakenOver(first, second)
	})
}

// frozenHolderTakenOver runs the spec: two Treebinds on one claim, the first one's agent frozen until the second one has the lock,
// then thawed, and the second Treebind deleted.
func frozenHolderTakenOver(first, second syncTarget) {
	syncedAtSeed(first)
	before := first.syncPod(Default)
	var epochFirst uint64
	Eventually(func(g Gomega) { _, epochFirst = lockTaken(g, first, false) }).Should(Succeed())

	By("applying a second Treebind on the same claim and the same subPath, which the operator accepts")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(holderStandbyManifest(first, second))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the second Treebind")
	var standbyPod syncPodState
	Eventually(func(g Gomega) {
		standbyPod = second.syncPod(g)
		g.Expect(standbyPod.started).NotTo(BeZero(), "the pod of the second Treebind has not started its agent")
	}, dodRestartBound).Should(Succeed())

	expectWaitingAgent(first, second, epochFirst, standbyPod.started)

	By("starting the privileged pod of hack/inject.sh before the freeze, so that the freeze does not wait for it")
	output, err := utils.ThawContainer(first.namespace, first.syncPodName(), failureAgentContainer)
	Expect(err).NotTo(HaveOccurred(), "Failed to start the privileged pod: %s", output)

	By("freezing the agent that holds the lock, and writing a file in the pod of the application while it is frozen")
	frozen := time.Now()
	output, err = utils.FreezeContainer(first.namespace, first.syncPodName(), failureAgentContainer)
	Expect(err).NotTo(HaveOccurred(), "Failed to freeze the agent")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)
	first.appWrite(frozenAppFile, frozenAppText)
	// The holder made no slot since it was frozen, so the top slot is its last, and no contender may take the lock a TTL after it.
	last := topSlot(holderSlots(Default))
	Expect(last.heldBy(first)).To(BeTrue(), "the top slot of the lock is of %q, not of the frozen agent", last.Holder)

	By("waiting for the second agent to take the lock over, with a higher epoch")
	var tookOver time.Time
	var epochSecond uint64
	Eventually(func(g Gomega) {
		tookOver, epochSecond = lockTaken(g, second, false)
	}, holderTakeoverBound, time.Second).Should(Succeed())

	By("thawing the first agent as soon as the takeover is seen")
	seen := time.Now()
	output, err = utils.ThawContainer(first.namespace, first.syncPodName(), failureAgentContainer)
	thawed := time.Now()
	Expect(err).NotTo(HaveOccurred(), "Failed to thaw the agent")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)
	Expect(epochSecond).To(BeNumerically(">", epochFirst), "the second agent took the lock with an epoch that is not higher")
	// The second agent saw that slot for the first time after it was made, and waits a TTL of its own clock from then. It may have
	// seen it before the freeze, so the takeover can come under a TTL after the freeze and not under one after the slot.
	Expect(tookOver.Sub(last.Time)).To(BeNumerically(">=", failureLockTTL),
		"the second agent took the lock %s after the last slot of the holder, under the TTL", tookOver.Sub(last.Time))
	_, _ = fmt.Fprintf(GinkgoWriter, "the last slot of the first agent was made %s before the freeze; the second agent took the lock "+
		"%s after the freeze, %s after that slot, from epoch %d to %d; it was seen %s later and the first agent was thawed %s after "+
		"the freeze\n", frozen.Sub(last.Time).Round(time.Millisecond), tookOver.Sub(frozen).Round(time.Millisecond),
		tookOver.Sub(last.Time).Round(time.Millisecond), epochFirst, epochSecond, seen.Sub(tookOver).Round(time.Millisecond),
		thawed.Sub(frozen).Round(time.Millisecond))

	By("waiting for the kubelet to start the first agent again, in the same pod, once")
	var after syncPodState
	Eventually(func(g Gomega) {
		after = first.syncPod(g)
		g.Expect(after.uid).To(Equal(before.uid), "the pod was replaced, and not its container started again")
		g.Expect(after.restarts).To(Equal(before.restarts+1), "the container was not started again, or more than once")
		g.Expect(after.started).To(BeTemporally(">", frozen), "the container that runs is not a new one")
	}, failureKillBound).Should(Succeed())
	expectHolderWokeUp(first, frozen)

	By("seeing the file that the application wrote while the first agent was frozen on the remote, added once")
	Eventually(func(g Gomega) {
		g.Expect(first.remoteFiles(g)).To(ConsistOf(failureSeedFile, frozenAppFile))
	}, dodBound).Should(Succeed())
	Expect(first.remoteCommitsAdding(frozenAppFile)).To(HaveLen(1), "the history of the remote adds the file more than once")
	Eventually(func(g Gomega) { second.expectSyncedAtTip(g, frozen) }, dodReportBound).Should(Succeed())

	By("checking that the first agent waits for the lock as the standby, and the second one holds it")
	state := first.syncPod(Default)
	Expect(state.ready).To(BeFalse(), "the first agent is ready without the lock")
	Expect(first.agentLog(Default)).NotTo(MatchRegexp(`check=Lock outcome=Passed`), "the first agent took the lock back")
	top := topSlot(holderSlots(Default))
	Expect(top.heldBy(second)).To(BeTrue(), "the top slot of the lock is of %q, not of the second agent", top.Holder)
	Expect(top.Epoch).To(Equal(epochSecond))
	// The slot after the last one of the frozen agent is where the second one's tenure began, so the frozen agent could not make a
	// slot, a heartbeat or a release, after it woke: none of its slots is above the epoch of the second agent.
	for _, slot := range holderSlots(Default) {
		if slot.Holder == last.Holder {
			Expect(slot.number).To(BeNumerically("<", epochSecond), "the frozen agent made the slot %d after the takeover", slot.number)
		}
	}
	Expect(second.syncPod(Default).restarts).To(BeZero(), "the second agent was started again")

	By("deleting the second Treebind, which gives the lock up when its pod stops, and writing a file on each side")
	deleted := time.Now()
	_, err = utils.Run(exec.Command("kubectl", "delete", "treebind", second.name, "-n", second.namespace, "--wait=true"))
	Expect(err).NotTo(HaveOccurred(), "Failed to delete the second Treebind")
	// The files are written once the second agent is gone, so that it does not commit the one of the application and the first
	// agent, which may hold the lock by now, has nothing to push while the commit from Git is made.
	Eventually(func(g Gomega) {
		g.Expect(second.syncPod(g).uid).To(BeEmpty(), "the pod of the second Treebind is still there")
	}, holderReleaseBound).Should(Succeed())
	first.pushFromGit(releasedGitFile, releasedGitText)
	first.appWrite(releasedAppFile, releasedAppText)

	By("waiting for the first agent to take the lock from the release of the second, with a higher epoch")
	var tookBack time.Time
	var epochThird uint64
	Eventually(func(g Gomega) {
		tookBack, epochThird = lockTaken(g, first, false)
	}, holderReleaseBound).Should(Succeed())
	Expect(epochThird).To(BeNumerically(">", epochSecond), "the first agent took the lock back with an epoch that is not higher")
	// A holder that is gone without giving the lock up is waited for a TTL, and the second agent gives it up when it is stopped, so
	// the first one follows the release at its next look: it does not wait for a TTL.
	Expect(tookBack.Sub(deleted)).To(BeNumerically("<", failureLockTTL),
		"the first agent took the lock %s after the second Treebind was deleted, which is a TTL or more", tookBack.Sub(deleted))

	By("seeing both files on both sides, the lock and the Git directory in order, and Synced at the tip of the remote")
	Eventually(func(g Gomega) {
		g.Expect(first.remoteFiles(g)).To(ConsistOf(failureSeedFile, frozenAppFile, releasedAppFile, releasedGitFile))
		g.Expect(first.appFile(g, releasedGitFile)).To(Equal(releasedGitText))
	}, dodBound).Should(Succeed())
	Eventually(func(g Gomega) { first.expectSyncedAtTip(g, deleted) }, dodReportBound).Should(Succeed())
	expectLinearHistory(first)
	_, _ = fmt.Fprintf(GinkgoWriter, "the second Treebind was deleted, the first agent took the lock %s later, from epoch %d to %d\n",
		tookBack.Sub(deleted).Round(time.Second), epochSecond, epochThird)

	By("checking that the first container was started again once and no more")
	Expect(first.syncPod(Default).restarts).To(Equal(before.restarts+1), "the container was started again more than once")
}

// expectWaitingAgent checks that the agent of the second Treebind waits for the lock while the first agent runs, for longer than
// the TTL, and writes nothing: it takes no slot, it has no checkout, and the remote has the commit of the seed and no other. The
// operator accepts the second Treebind, and it is never ready.
func expectWaitingAgent(first, second syncTarget, epochFirst uint64, startedAt time.Time) {
	By("watching the second agent wait for the lock for longer than the TTL")
	seed := first.remoteTip(Default)
	watching := max(time.Until(startedAt.Add(holderWaitFor)), 10*time.Second)
	Consistently(func(g Gomega) {
		slots := holderSlots(g)
		g.Expect(slots).NotTo(BeEmpty())
		for _, slot := range slots {
			g.Expect(slot.heldBy(first)).To(BeTrue(), "the slot %d of the lock is of %q, not of the first agent", slot.number, slot.Holder)
			g.Expect(slot.Epoch).To(Equal(epochFirst))
		}
		g.Expect(first.remoteTip(g)).To(Equal(seed), "the remote has a commit that the seed does not")
		log := second.agentLog(g)
		g.Expect(log).NotTo(MatchRegexp(`check=(Lock|Checkout|Pull|Commit|Push) `), "the second agent did more than wait: %s", log)
		g.Expect(second.sync(g).ready).NotTo(Equal("True"), "the second Treebind is ready, with its agent waiting for the lock")
	}, watching, 2*time.Second).Should(Succeed())
	// The operator says what the agent reports of a step that has not come yet: not ready, and a sync that is pending.
	var state treebindSyncState
	Eventually(func(g Gomega) {
		state = second.sync(g)
		g.Expect(state.ready).To(Equal("False"))
		g.Expect(state.synced).To(Equal("Unknown"))
		g.Expect(state.syncedReason).To(Equal("SyncPending"))
	}, dodReportBound).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "the second Treebind, %s after its agent started: Ready %q, Synced %q (%s), lock slots %v\n",
		since(startedAt), state.ready, state.synced, state.syncedReason, holderSlots(Default))
}

// expectHolderWokeUp reads how the container of the first agent ended after the freeze, and checks what it did when it woke: it
// ended by itself, it said why, and it wrote nothing. It says which way it went, as the comment on exitLockLost says.
func expectHolderWokeUp(first syncTarget, frozen time.Time) {
	By("checking what the first agent did when it was thawed")
	output, err := utils.Run(exec.Command("kubectl", "get", "pod", first.syncPodName(), "-n", first.namespace, "-o",
		"jsonpath={.status.containerStatuses[0].lastState.terminated.exitCode}|"+
			"{.status.containerStatuses[0].lastState.terminated.reason}"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the first Treebind")
	ended := strings.Split(strings.TrimSpace(output), "|")
	Expect(ended).To(HaveLen(2), "kubectl answered %q", output)
	events := podEvents(first)
	logs, err := utils.Run(exec.Command("kubectl", "logs", first.syncPodName(), "-n", first.namespace, "--previous"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the log of the first container")

	lost := "the writer lock was lost to another agent, and this one stops"
	stopped := "the agent stopped on request"
	if strings.Contains(events, "failed liveness probe") {
		_, _ = fmt.Fprintf(GinkgoWriter, "the kubelet had asked the container to stop when it was thawed: it ended with the code %s (%s)\n",
			ended[0], ended[1])
		Expect(ended[0]).To(Or(Equal(exitLockLost), Equal(exitStopped)), "the container did not end by itself when it was thawed")
		Expect(logs).To(Or(ContainSubstring(lost), ContainSubstring(stopped)), "the agent did not say why it stopped")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "the probe had not failed three times when it was thawed: it ended with the code %s (%s)\n",
			ended[0], ended[1])
		Expect(ended[0]).To(Equal(exitLockLost), "the agent did not stop with the code of a lost lock")
		Expect(logs).To(ContainSubstring(lost), "the agent did not say that the lock was lost")
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "the events of its pod:\n%s\nthe log of the first container:\n%s\n", events, logs)

	// What the agent writes shows in its log, in the check of each step (internal/agent/app): after the freeze it has none of them.
	for _, line := range strings.Split(logs, "\n") {
		if at, ok := logTime(line); ok && at.After(frozen) {
			Expect(line).NotTo(MatchRegexp(`check=(Checkout|Pull|Commit|Push|Conflict)`), "the first agent did a step after it was frozen")
		}
	}
}

// logTime is the time of a line of the log of the agent.
func logTime(line string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(line, "time=")
	if !ok {
		return time.Time{}, false
	}
	field, _, _ := strings.Cut(rest, " ")
	at, err := time.Parse(time.RFC3339, field)
	return at, err == nil
}

// expectLinearHistory checks that the history of the remote is a line in which each file was added once, that no lock file is left
// on the volume, and that the repository of the remote is whole.
func expectLinearHistory(t syncTarget) {
	By("checking that the history of the remote is a line, with each file added once")
	merges, err := remoteShell("git -C " + t.repository + " rev-list --merges --count " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to read the history of the repository")
	Expect(merges).To(Equal("0"), "the history of the remote has a merge")
	for _, name := range []string{failureSeedFile, frozenAppFile, releasedAppFile, releasedGitFile} {
		Expect(t.remoteCommitsAdding(name)).To(HaveLen(1), "the history of the remote adds %s more than once, or never", name)
	}
	commits, err := remoteShell("git -C " + t.repository + " rev-list --count " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to read the history of the repository")
	Expect(commits).To(Equal("4"), "the history of the remote has a commit more than the four files need")
	t.expectRemoteFsck()

	By("checking that no lock file and no journal of a step is left on the volume")
	litter, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", holderPeek, "--", "find", peekMount, "(", "-name", "*.lock",
		"-o", "-name", "commit-paths", "-o", "-name", "updating", ")"))
	Expect(err).NotTo(HaveOccurred(), "Failed to look for lock files on the volume")
	Expect(utils.GetNonEmptyLines(litter)).To(BeEmpty(), "files of a step are left on the volume")
}

// holderSlots lists the slots of the writer lock on the volume, in the order of their numbers.
func holderSlots(g Gomega) []lockSlot {
	output, err := utils.Run(exec.Command("kubectl", "-n", failureNamespace, "exec", holderPeek, "--", "sh", "-c",
		"cd "+peekLockDir+` && for f in slot-*; do r="$(cat "$f" 2>/dev/null)" || continue; echo "${f#slot-} $r"; done`))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the lock on the volume")
	var slots []lockSlot
	for _, line := range utils.GetNonEmptyLines(output) {
		number, record, _ := strings.Cut(line, " ")
		slot := lockSlot{}
		var err error
		slot.number, err = strconv.ParseUint(number, 10, 64)
		g.Expect(err).NotTo(HaveOccurred(), "the name of a slot is %q", number)
		// A slot is made before its record is written, so one may be empty for a moment.
		if record != "" {
			g.Expect(json.Unmarshal([]byte(record), &slot)).To(Succeed(), "the record of the slot %d is %q", slot.number, record)
		}
		slots = append(slots, slot)
	}
	return slots
}

// topSlot is the slot of the highest number, whose maker holds the lock, or the zero slot when there is none.
func topSlot(slots []lockSlot) lockSlot {
	var top lockSlot
	for _, slot := range slots {
		if slot.number > top.number {
			top = slot
		}
	}
	return top
}

// podEvents lists the events of the pod of the sync as one line each: the reason, how often, when it last happened and what it says.
func podEvents(t syncTarget) string {
	output, err := utils.Run(exec.Command("kubectl", "get", "events", "-n", t.namespace, "--field-selector",
		"involvedObject.name="+t.syncPodName(), "--sort-by=.lastTimestamp", "-o",
		`jsonpath={range .items[*]}{.lastTimestamp} {.reason} x{.count}: {.message}{"\n"}{end}`))
	if err != nil {
		return fmt.Sprintf("the events could not be read: %v", err)
	}
	return output
}

// holderPeekManifest is the pod that looks at the claim of a target.
func holderPeekManifest(t syncTarget) string {
	return `apiVersion: v1
kind: Pod
metadata:
  name: ` + holderPeek + `
  namespace: ` + t.namespace + `
spec:
  containers:
  - name: peek
    image: ` + dodAppImage + `
    imagePullPolicy: IfNotPresent
    command: ["sleep", "3600"]
    volumeMounts:
    - {name: data, mountPath: ` + peekMount + `, readOnly: true}
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: ` + t.name + `-data
`
}

// holderStandbyManifest is a Treebind of its own on the claim of the first target, with the same repository, branch, subPath and
// timings.
func holderStandbyManifest(first, second syncTarget) string {
	return `apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: ` + second.name + `
  namespace: ` + second.namespace + `
spec:
  repository:
    url: ` + remoteSSH + first.repository + `
    branch: ` + first.branch + `
    auth:
      sshKeySecretRef: {name: dev-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: dev-known-hosts, key: known_hosts}
  target:
    claimName: ` + first.name + `-data
    subPath: ` + dodSubPath + `
  sync:
    direction: bidirectional
    interval: ` + dodInterval + `
    settleTime: ` + dodSettleTime + `
`
}

// tearDownHolder removes what the spec made: the second Treebind and the pod that looks at the claim go first, because the claim is
// not removed while a pod uses it, and then what setUpFailure made.
func tearDownHolder(first, second syncTarget) {
	By("removing the second Treebind and the pod that looks at the claim")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", first.namespace, "treebind/"+second.name, "pod/"+holderPeek,
		"--ignore-not-found", "--grace-period=0", "--force"))
	tearDownFailure(first)
}

// diagnoseHolder writes to the log of the spec, when it failed, what tells why: what diagnoseFailure writes of the first Treebind,
// the same of the second, the events of both pods and the slots of the lock.
func diagnoseHolder(first, second syncTarget) {
	if !CurrentSpecReport().Failed() {
		return
	}
	diagnoseFailure(first)
	diagnoseFailure(second)
	_, _ = fmt.Fprintf(GinkgoWriter, "the events of the pod of the first Treebind:\n%s\nthe events of the pod of the second:\n%s\n",
		podEvents(first), podEvents(second))
	slots := holderSlots(Default)
	_, _ = fmt.Fprintf(GinkgoWriter, "the slots of the lock: %+v\n", slots)
}
