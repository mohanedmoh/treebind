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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/mohanedmoh/treebind/internal/agent/syncloop"
	"github.com/mohanedmoh/treebind/test/utils"
)

// The kills in the middle of a step of Git. The agent runs no hook and no filter, so the moment is told by files that the agent
// and Git make on the volume, which hack/inject.sh watches for and acts on only when they still say so once what runs is
// stopped: the agent's container frozen from the node, or the Git itself stopped from inside the container. A burst of a few
// thousand small files keeps each moment a good part of a second long. What each kill leaves, and what the next start does with
// it, is in docs/adr/0028; the checkout is judged by what the agent does with it next, and not by Git run by hand in the agent's Git
// directory, which ADR 0019 and ADR 0028 tell people not to do.
const (
	// agentMount is where the pod of the sync mounts the claim (the default of target.mountPath). The markers are under it.
	agentMount    = "/work"
	agentStateDir = agentMount + "/.treebind"
	// indexLock is the lock of the index, which Git holds while it writes the index and renames into place when it is done, and
	// commitPaths the file of the paths of a commit, which the agent writes just before git add and removes just after
	// (internal/agent/commit). With both there, git add is in the middle.
	indexLock   = agentStateDir + "/git/index.lock"
	commitPaths = agentStateDir + "/commit-paths"
	// updateJournal is there from just before the files of an update are written until just after (internal/agent/pull).
	updateJournal = agentStateDir + "/updating"

	// burstFiles small files are written in burstDir at once. On a laptop Git adds 3000 of them in about 0.4 seconds and writes
	// them in a fast-forward in about 0.15 seconds, and on kind there the watch looked every 4 to 7 microseconds. With their
	// objects they take about 25 MB of the volume. Their names are numbers of one width from burstFirst, so that the order of the
	// names, in which Git writes them, is the order of the numbers.
	burstFiles = 3000
	burstFirst = 10000
	burstDir   = "burst"

	// momentBound is how long the script watches for its moment: the scan of the files and their settle time, or the interval of
	// the pull, and the moments that pass while the container freezes or the Git stops.
	momentBound = 3 * time.Minute

	// gitAddWord is in the arguments of the git add of a commit, and of no other Git that the agent runs.
	gitAddWord = "--pathspec-from-file="
)

var (
	// checkoutRemovedIndexLock is the line in which the checkout of a new agent says that it removed the lock of the index.
	checkoutRemovedIndexLock = regexp.MustCompile(`(?m)^time=\S+ level=INFO msg=checkout check=Checkout outcome=Passed ` +
		`.*removed the lock file that a Git command that was stopped left: index\.lock`)
	// updateUndone is the line in which a pull says that it undid an update that was cut off, and how many of its files it removed,
	// before it applied it again.
	updateUndone = regexp.MustCompile(`(?m)^time=\S+ level=INFO msg=pull check=Pull outcome=Passed .*the update from \S+ to \S+ ` +
		`was cut off, and was undone, and the next pull applies it again: \d+ file\(s\) put back, (\d+) removed`)
	// commitFailedInAdd is the line in which the commit step says that its git add failed.
	commitFailedInAdd = regexp.MustCompile(`(?m)^time=\S+ level=WARN msg=commit check=Commit outcome=Failed reason=CommitFailed ` +
		`.*git add failed`)
	// stepRemovedIndexLock is the line in which a pull or a commit says that it removed the lock of the index first.
	stepRemovedIndexLock = regexp.MustCompile(`(?m)^time=\S+ level=INFO msg=(pull|commit) check=(Pull|Commit) outcome=Passed ` +
		`.*removed the lock file that a Git command that was stopped left: index\.lock`)
	// commitPassed is the line in which the commit step says that it made a commit.
	commitPassed = regexp.MustCompile(`(?m)^time=\S+ level=INFO msg=commit check=Commit outcome=Passed .*committed \d+ files? as`)
)

// killMidStep declares the Contexts of the kills in the middle of a step, in the Context of the failure specs.
func killMidStep() {
	Context("when the agent is killed in the middle of git add", func() {
		target := failureTarget("kill-add")
		BeforeAll(func() { setUpFailure(target) })
		AfterAll(func() { tearDownFailure(target) })
		AfterEach(func() { afterKill(target) })

		It("should remove the lock of the index when it starts again and commit each file once", func() {
			killInGitAdd(target)
		})
	})

	Context("when the agent is killed in the middle of the files of an update", func() {
		// The first scan of a start comes at once and the first pull after a phase that is drawn from the name of the Treebind
		// (syncloop.Phase), and it is the pull that undoes an update that was cut off. When the phase is longer than the settle
		// time, the scan finds the files that the update had written and commits them as the application's first: on kind, with
		// the name mid-update and its phase of 3.9s, the new agent committed the 12 files that the update had written, the last of
		// them half, and its pull resolved a conflict by followRemote and removed the journal without undoing anything. That is
		// for the agent to put right (#232), and when it is the name of this Treebind needs no short phase. This spec is for the
		// undo, so the phase of its name is under the settle time.
		target := failureTarget("kill-update")
		BeforeAll(func() {
			expectPullBeforeSettle(target)
			setUpFailure(target)
		})
		AfterAll(func() { tearDownFailure(target) })
		AfterEach(func() { afterKill(target) })

		It("should undo the update when it starts again and apply it whole", func() {
			killInUpdate(target)
		})
	})

	Context("when a Git that the agent started is killed in the middle of git add", func() {
		target := failureTarget("kill-git")
		BeforeAll(func() { setUpFailure(target) })
		AfterAll(func() { tearDownFailure(target) })
		AfterEach(func() { afterKill(target) })

		It("should fail that commit, remove the lock of the index and commit each file once, in the same container", func() {
			killGitInAdd(target)
		})
	})
}

// killInGitAdd kills the agent, from the node, while its git add stages a burst of files that the application wrote, and checks
// that the container comes back once in the same pod, that the new agent removes the lock of the index that git add left, and
// that each file is added by one commit on the remote.
func killInGitAdd(t syncTarget) {
	before := syncedAtSeed(t)

	By("watching from the node for git add of a commit, and writing a burst of files in the pod of the application")
	watch, err := utils.KillAt(t.namespace, t.syncPodName(), failureAgentContainer, momentBound, "+"+indexLock, "+"+commitPaths)
	Expect(err).NotTo(HaveOccurred(), "Failed to watch for git add")
	t.appWriteBurst()
	awaitStrike(watch, "the agent was not killed in the middle of git add")
	killed := time.Now()

	t.restartedOnceAfterKill(before)

	By("checking in its log that the new agent removed the lock of the index that git add left")
	Expect(t.agentLog(Default)).To(MatchRegexp(checkoutRemovedIndexLock.String()))

	expectBurstCommittedOnce(t, killed)
	t.expectRestarts(before, 1)
}

// killInUpdate kills the agent, from the node, while the fast-forward of a pull writes a burst of files that was pushed from Git,
// with the first of them written and the last not yet, and checks that the container comes back once in the same pod, that the
// new agent undoes the update that was cut off and applies it again, and that each file in the pod of the application is whole.
func killInUpdate(t syncTarget) {
	before := syncedAtSeed(t)

	By("watching from the node for the files of an update, and pushing a burst of files from Git")
	first, last := burstName(burstFirst), burstName(burstFirst+burstFiles-1)
	watch, err := utils.KillAt(t.namespace, t.syncPodName(), failureAgentContainer, momentBound, "+"+updateJournal,
		"+"+agentMount+"/"+dodSubPath+"/"+first, "-"+agentMount+"/"+dodSubPath+"/"+last)
	Expect(err).NotTo(HaveOccurred(), "Failed to watch for the files of an update")
	pushed := time.Now()
	t.pushBurstFromGit()
	awaitStrike(watch, "the agent was not killed in the middle of the files of an update")

	t.restartedOnceAfterKill(before)

	By("checking in its log that the new agent found the update cut off and undid it before it applied it again")
	match := updateUndone.FindStringSubmatch(t.agentLog(Default))
	Expect(match).NotTo(BeNil(), "the new agent did not say that it undid an update that was cut off")
	removed, err := strconv.Atoi(match[1])
	Expect(err).NotTo(HaveOccurred())
	// The files that the update had written, and the one it was writing, were the agent's own, and were removed.
	Expect(removed).To(BeNumerically(">", 0), "the update that was cut off had written no file")
	Expect(removed).To(BeNumerically("<", burstFiles), "the update that was cut off had written every file")

	By("seeing every file in the pod of the application whole, by its checksum against the remote's")
	Eventually(func(g Gomega) {
		g.Expect(differences(t.appChecksums(g), t.remoteChecksums(g))).To(BeEmpty())
	}, dodBound).Should(Succeed())

	By("seeing Synced say that the volume and the repository agree at the tip of the remote, and no lock file left")
	Eventually(func(g Gomega) {
		t.expectSyncedAtTip(g, pushed)
	}, dodReportBound).Should(Succeed())
	Eventually(func(g Gomega) { g.Expect(t.lockFiles(g)).To(BeEmpty(), "a lock file is left") }).Should(Succeed())
	t.expectRemoteFsck()
	t.expectRestarts(before, 1)
}

// killGitInAdd kills the git add of a commit, from inside the container of the agent, while the agent runs, as the out-of-memory
// killer can pick the largest process of a container, and checks that the commit fails, that a later step removes the lock of the
// index that git add left, and that each file is added by one commit on the remote, without the container being started again.
func killGitInAdd(t syncTarget) {
	before := syncedAtSeed(t)

	By("watching in the container of the agent for its git add, and writing a burst of files in the pod of the application")
	watch, err := utils.KillGitAt(t.namespace, t.syncPodName(), failureAgentContainer, momentBound, gitAddWord,
		"+"+indexLock, "+"+commitPaths)
	Expect(err).NotTo(HaveOccurred(), "Failed to watch for git add")
	t.appWriteBurst()
	awaitStrike(watch, "the git add of the agent was not killed")
	killed := time.Now()

	By("checking in its log that the commit failed, that a later step removed the lock of the index, and that a commit was made")
	Eventually(func(g Gomega) {
		logs := t.agentLog(g)
		failed := commitFailedInAdd.FindStringIndex(logs)
		g.Expect(failed).NotTo(BeNil(), "the commit step did not say that its git add failed")
		logs = logs[failed[1]:]
		removed := stepRemovedIndexLock.FindStringIndex(logs)
		g.Expect(removed).NotTo(BeNil(), "no step said that it removed the lock of the index after the commit failed")
		// The commit that removed the lock says so in the line of the commit it made.
		g.Expect(commitPassed.MatchString(logs[removed[0]:])).To(BeTrue(), "no commit was made after the lock was removed")
	}, dodRestartBound).Should(Succeed())

	expectBurstCommittedOnce(t, killed)
	t.expectRestarts(before, 0)
}

// syncedAtSeed waits for the Treebind to be ready with the seeded file in the pod of the application and Synced at the seeded
// commit, so that the agent has nothing to do, and returns the pod of the sync as it is then.
func syncedAtSeed(t syncTarget) syncPodState {
	By("waiting for the Treebind to be ready, with the seeded file in the pod of the application")
	Eventually(func(g Gomega) {
		g.Expect(t.sync(g).ready).To(Equal("True"))
		g.Expect(t.appFile(g, failureSeedFile)).To(Equal("hello"))
	}, dodRestartBound).Should(Succeed())

	By("waiting for Synced to say that the volume and the repository agree at the seeded commit")
	Eventually(func(g Gomega) {
		t.expectSyncedAtTip(g, time.Time{})
	}, dodReportBound).Should(Succeed())

	var before syncPodState
	Eventually(func(g Gomega) {
		before = t.syncPod(g)
		g.Expect(before.ready).To(BeTrue())
	}).Should(Succeed())
	return before
}

// expectPullBeforeSettle checks that the first pull of the Treebind's agent comes before a file that its first scan finds has
// settled.
func expectPullBeforeSettle(t syncTarget) {
	interval, err := time.ParseDuration(dodInterval)
	Expect(err).NotTo(HaveOccurred())
	settle, err := time.ParseDuration(dodSettleTime)
	Expect(err).NotTo(HaveOccurred())
	Expect(syncloop.Phase(t.namespace, t.name, interval)).To(BeNumerically("<", settle),
		"the first pull of %s comes after its first scan has committed the files of an update that was cut off", t.name)
}

// awaitStrike waits for a watch of hack/inject.sh to strike, and logs what it saw.
func awaitStrike(watch *utils.Watch, failure string) {
	output, err := watch.Wait(momentBound + time.Minute)
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)
	Expect(err).NotTo(HaveOccurred(), failure)
}

// expectBurstCommittedOnce checks that every file of the burst is on the remote, each added by one commit and no file twice, that
// the Treebind says that it is synced at the tip, that no lock file is left in the agent's Git directory and that the remote's
// repository is whole.
func expectBurstCommittedOnce(t syncTarget, notBefore time.Time) {
	By("seeing each file of the burst added by one commit on the remote")
	Eventually(func(g Gomega) {
		output, err := remoteShell("git -C " + t.repository + " log --diff-filter=A --name-only --format= " + t.branch + " -- " +
			burstDir)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to read the history of the repository")
		added := map[string]int{}
		for _, name := range utils.GetNonEmptyLines(output) {
			added[name]++
		}
		for i := burstFirst; i < burstFirst+burstFiles; i++ {
			g.Expect(added).To(HaveKeyWithValue(burstName(i), 1), "the file is not on the remote, or was added more than once")
		}
		g.Expect(added).To(HaveLen(burstFiles))
	}, dodBound).Should(Succeed())

	By("seeing Synced say that the volume and the repository agree at the tip of the remote, and no lock file left")
	Eventually(func(g Gomega) {
		t.expectSyncedAtTip(g, notBefore)
	}, dodReportBound).Should(Succeed())
	Eventually(func(g Gomega) { g.Expect(t.lockFiles(g)).To(BeEmpty(), "a lock file is left") }).Should(Succeed())
	t.expectRemoteFsck()
}

// afterKill thaws the container of the agent when a spec failed, because a watch that was stopped may have left it frozen, and
// writes what tells why the spec failed.
func afterKill(t syncTarget) {
	if !CurrentSpecReport().Failed() {
		return
	}
	output, err := utils.ThawContainer(t.namespace, t.syncPodName(), failureAgentContainer)
	_, _ = fmt.Fprintf(GinkgoWriter, "thawing the agent: %s (%v)\n", output, err)
	diagnoseFailure(t)
}

// burstName is the path of a file of the burst, in the synced directory.
func burstName(i int) string { return fmt.Sprintf("%s/%d.txt", burstDir, i) }

// burstScript is the shell that writes the files of the burst in the directory that it runs in, each with its own content.
func burstScript() string {
	return fmt.Sprintf(`mkdir -p %s && i=%d && while [ "$i" -lt %d ]; do echo "file $i of the burst" > %s/"$i".txt; i=$((i + 1)); done`,
		burstDir, burstFirst, burstFirst+burstFiles, burstDir)
}

// appWriteBurst writes the burst in the directory of the claim that the pod of the application mounts, as the application does.
func (t syncTarget) appWriteBurst() {
	_, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "sh", "-c",
		"cd "+t.appMount+" && "+burstScript()))
	Expect(err).NotTo(HaveOccurred(), "Failed to write the burst in the pod of the application")
}

// pushBurstFromGit makes a commit with the burst in the clone that the seeding made, after taking what the repository has, and
// pushes it.
func (t syncTarget) pushBurstFromGit() {
	_, err := remoteShell("cd " + t.clone + " && git pull -q " + t.repository + " " + t.branch + " && " + burstScript() +
		" && git add -A && git commit -q -m 'from git: a burst' && git push -q " + t.repository + " " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to push the burst from Git")
}

// restartedOnceAfterKill waits for the kubelet to start the agent that was killed again in the same pod, once, and for the pod to
// be ready, and checks that the container that was killed ended with SIGKILL.
func (t syncTarget) restartedOnceAfterKill(before syncPodState) {
	By("waiting for the kubelet to start the agent again in the same pod, and for the pod to be ready")
	Eventually(func(g Gomega) {
		after := t.syncPod(g)
		g.Expect(after.uid).To(Equal(before.uid), "the pod was replaced, and not its container started again")
		g.Expect(after.restarts).To(Equal(before.restarts+1), "the container was not started again, or more than once")
		g.Expect(after.ready).To(BeTrue())
	}, failureKillBound).Should(Succeed())
	exitCode, err := utils.Run(exec.Command("kubectl", "get", "pod", t.syncPodName(), "-n", t.namespace,
		"-o", "jsonpath={.status.containerStatuses[0].lastState.terminated.exitCode}"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the sync")
	Expect(exitCode).To(Equal("137"), "the container that was killed did not end with SIGKILL")
}

// expectRestarts checks that the pod of the sync is the one it was, ready, with its container started again as many times more as
// given, so that the agent does not loop (R-8).
func (t syncTarget) expectRestarts(before syncPodState, more int) {
	By(fmt.Sprintf("checking that the container was started again %d time(s) and no more, in the same pod", more))
	state := t.syncPod(Default)
	Expect(state.uid).To(Equal(before.uid), "the pod was replaced")
	Expect(state.restarts).To(Equal(before.restarts+more), "the container was started again %d time(s)", state.restarts-before.restarts)
	Expect(state.ready).To(BeTrue(), "the pod is not ready")
}

// agentLog is the log of the agent of the container that runs.
func (t syncTarget) agentLog(g Gomega) string {
	logs, err := utils.Run(exec.Command("kubectl", "logs", t.syncPodName(), "-n", t.namespace, "-c", failureAgentContainer))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the log of the agent")
	return logs
}

// lockFiles lists the lock files in the agent's Git directory, at its top and under refs, where Git makes them. It looks with the
// shell of the agent's image, whose test is built in, and runs no Git there.
func (t syncTarget) lockFiles(g Gomega) []string {
	git := agentStateDir + "/git"
	output, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.syncPodName(), "-c", failureAgentContainer,
		"--", "sh", "-c", "for f in "+git+"/*.lock "+git+"/refs/*/*.lock "+git+"/refs/*/*/*.lock "+git+"/refs/*/*/*/*.lock; "+
			`do if [ -e "$f" ]; then echo "$f"; fi; done`))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to look for lock files in the pod of the sync")
	return utils.GetNonEmptyLines(output)
}

// appChecksums lists the checksum and the path of every file in the pod of the application, in the order of the paths.
func (t syncTarget) appChecksums(g Gomega) []string {
	output, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "sh", "-c",
		"cd "+t.appMount+" && find . -type f | sort | xargs sha256sum"))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the files in the pod of the application")
	return utils.GetNonEmptyLines(output)
}

// remoteChecksums lists the checksum and the path of every file of the branch of the remote, as appChecksums does, from an
// archive of the branch.
func (t syncTarget) remoteChecksums(g Gomega) []string {
	output, err := remoteShell(`d="$(mktemp -d)" && trap 'rm -rf "$d"' EXIT && git -C ` + t.repository + " archive " + t.branch +
		` | tar -x -C "$d" && cd "$d" && find . -type f | sort | xargs sha256sum`)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read the files of the repository")
	return utils.GetNonEmptyLines(output)
}

// expectRemoteFsck checks that the repository of the remote is whole.
func (t syncTarget) expectRemoteFsck() {
	By("checking the repository of the remote with git fsck")
	output, err := remoteShell("git -C " + t.repository + " fsck --no-progress")
	Expect(err).NotTo(HaveOccurred(), "git fsck found the repository of the remote damaged: %s", output)
}

// differences lists the first lines that are in one list and not in the other, each with the side it is on.
func differences(app, remote []string) []string {
	const most = 10
	inApp, inRemote := map[string]bool{}, map[string]bool{}
	for _, line := range app {
		inApp[line] = true
	}
	for _, line := range remote {
		inRemote[line] = true
	}
	var found []string
	for _, line := range app {
		if !inRemote[line] && len(found) < most {
			found = append(found, "only in the pod of the application: "+line)
		}
	}
	for _, line := range remote {
		if !inApp[line] && len(found) < most {
			found = append(found, "only on the remote: "+line)
		}
	}
	return found
}
