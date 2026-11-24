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
	"bytes"
	"crypto/sha256"
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

// A full volume is made on a tmpfs and never on a claim that kind makes. local-path makes a directory on the filesystem of the node,
// which on kind is the disk of the machine that runs the suite, and does not hold a claim to its size: filling the claim fills that
// disk. So the volume of these specs is a tmpfs of 16 MiB that hack/inject.sh mounts in the mount namespace of the node, a static
// PersistentVolume of type local on its directory, with a storage class name that no provisioner serves, and a claim that names that
// volume. hack/inject.sh refuses to write anywhere that is not the mount point of such a tmpfs, and never writes more than it holds. The
// filler is written by the privileged pod, at the root of the claim, outside the subPath of the Treebind and outside .treebind, so that
// the agent does not sync it. A tmpfs is memory, and its pages count against the memory of whoever wrote them, which is one more reason
// for it to be small and for the privileged pod, and not the agent, whose limit is 128 MiB, to write the filler.
//
// What the specs assert is what the agent does today on a full volume. The check before writing that #78 (M2) asks for makes a volume
// that is about to be full a condition at once, with a reason of its own, and not a pull that fails, a heartbeat of the writer lock that
// cannot be written and a restart, so the assertions on the reasons, on the restart and on its exit code change when it lands.
const (
	// fullVolumeMiB is the size of the tmpfs, and fullStorageClass the class that the volume and its claim name, which no provisioner
	// serves and no StorageClass object defines: a claim that names a volume is bound to it without one.
	fullVolumeMiB    = 16
	fullStorageClass = "treebind-tmpfs"

	// fullBigFile is the file that is pushed from Git, of fullBigMiB mebibytes of one repeated letter: a few kilobytes to fetch, because
	// it compresses, and as many mebibytes to write as it is long. It is not zeros, which compress as well: Git writes a block of zeros
	// into a checkout as a hole, so that the update of a file of zeros passes and takes one page of the volume, and what fills the volume
	// is the scan after it, which reads the holes through a mapping that makes a page of each, until git status ends with SIGBUS.
	fullBigFile = "big.bin"
	fullBigMiB  = 4
	// fullRoom is what is left of the volume while the update runs: more than the objects of the update and the slots of the writer lock
	// need, and much less than the file. fullSlotRoom is what is left in the second spec: two blocks of the tmpfs, which hold the slot of
	// the writer lock that is made before the one it replaces is removed, and less than the objects of a commit.
	fullRoom     = 1 << 20
	fullSlotRoom = 2 * 4096

	// fullStopBound is how long the agent and the operator have, from the moment the volume is full to the last block, to stop the agent
	// and say why: the heartbeat counts the lock as lost 30 seconds after the last slot it could make and the agent exits, and the
	// container that follows fails the writable check, which the operator reads at its next poll, every 30 seconds, a fifth more or
	// less, and every minute after a poll that failed. On kind Synced said WriteFailed 49 to 59 seconds after the volume was full.
	fullStopBound = failureKillBound
	// fullFreedBound is how long the restarted agent has, from the moment the volume has room again, to bring the file whole: its
	// writable check tries again after 5 seconds, then after twice as long each time up to a minute, and once it passes the agent waits a
	// TTL of the lock for the slot of the container before and pulls. On kind the file was whole 40 to 47 seconds after.
	fullFreedBound = 3 * time.Minute
)

// fullVolume is the Context of the full volume, which failureInjection calls.
func fullVolume() {
	// The update that the full volume cuts off leaves the half of the file, which the scans take for a change of the application from
	// the first second after it, and the journal of the update, and the pull is what undoes the update. Before #232 was fixed, the
	// volume being full was all that stopped the agent that ran from committing that half, and the agent that starts again scanned
	// before its first pull, which comes after a phase that is drawn from the name of the Treebind (syncloop.Phase): a name whose phase
	// was longer than the settle time would have had the half file committed, so this Context had a name with a short one. The agent
	// now commits nothing while the journal is there, and its first pull after such a start does not wait for the phase
	// (docs/adr/0028), and the phase of this name is longer than the settle time, so that the spec holds by that order.
	target := failureTarget("full-claim")
	var node string
	BeforeAll(func() {
		expectPullAfterSettle(target)
		node = setUpFullVolume(target)
	})
	// The filler is freed whether the spec passed or not, once a spec that failed has said how the volume was, and then the claim, the
	// volume and the tmpfs go, before the privileged pod that mounted it does (the AfterAll of failureInjection).
	AfterEach(func() { afterFullVolume(target, node) })
	AfterAll(func() { tearDownFullVolume(target, node) })

	It("should stop an update in the middle of a file and restart the agent while the volume is full, and bring the file whole "+
		"when it is freed", func() {
		fillAndFree(target, node)
	})

	It("should say PullFailed for a fetch that has no room, and need no restart when there is room again in time", func() {
		fillBeforeObjects(target, node)
	})
}

// fillAndFree fills the volume until an update cannot be written, pushes a big file from Git so that the update stops in the middle
// of the file, fills the rest, and checks what the Treebind says and the agent does while the volume is full. Then it frees the volume
// and checks that the file in the pod of the application is whole, that a file written after it is on the remote, that the Treebind is
// synced at the tip and that the repository of the remote is whole.
//
// What the agent does, as seen on kind, from the push: the pull fails after 1 to 6 seconds as PullFailed, with the error of Git, and
// leaves the half of the file, and the volume is full to the last block, so that nothing is left to fill. The pull that follows says
// Refused, because the lock cannot be checked, the heartbeat counts the lock as lost 30 seconds after the last slot it made, and the
// agent exits with code 3 about 35 seconds after the push, with a line that says that the heartbeat of the writer lock could not be
// written, and the error of the volume beside it. The container that follows fails the writable check as WriteFailed, and tries again
// after 5, 10, 20, 40 and 60 seconds, without exiting. Synced says InSync until the operator reads the report, then Refused, then
// WriteFailed once the restarted agent reports, with Ready False; after the volume is freed it says SyncPending, and then InSync.
func fillAndFree(t syncTarget, node string) {
	before := syncedAtSeed(t)

	By("checking that the volume of the claim is a tmpfs, as the pod of the application sees it")
	filesystem, err := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "stat", "-f", "-c", "%T", t.appMount))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the filesystem of the claim")
	Expect(strings.TrimSpace(filesystem)).To(Equal("tmpfs"), "the claim of this spec is not on a tmpfs, and is not filled")

	By("filling the volume until less is left than the update needs")
	output, err := utils.FillTmpfs(node, t.name, fullRoom)
	Expect(err).NotTo(HaveOccurred(), "Failed to fill the volume")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)
	Expect(t.sync(Default).synced).To(Equal("True"), "the Treebind stopped being synced when the volume had room for little")

	By("pushing a big file from Git, whose update does not fit")
	begun := time.Now()
	t.pushBigFromGit()
	pushed := time.Now()

	// Git writes the file until the volume refuses it and leaves what it wrote, so that the volume is full to the last block, and the
	// pull says PullFailed with the error of Git. The pull comes next after the interval, and the agent's log is what says it: the
	// operator reads the report only every 30 seconds, and what it reads may be a later one.
	By("seeing the update stop in the middle of the file")
	var trace fullTrace
	var stopped string
	Eventually(func(g Gomega) {
		trace.look(g, t, node, begun)
		stopped = pullFailureSince(g, t, pushed)
	}).WithPolling(2 * time.Second).Should(Succeed())
	Expect(stopped).To(ContainSubstring("reason=PullFailed"))
	Expect(stopped).To(ContainSubstring(fullBigFile), "the pull failed for another file than the big one")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s after the push: the update stopped: %s\n", since(pushed), stopped)

	// Whatever Git freed when it gave up, the rest is filled, so that the volume is full and not one block short of it: a volume with a
	// block left keeps the lock, because each new slot lets an old one be removed, and leaves the pull to its back-off of up to ten
	// minutes. On kind Git left the file it was writing, and nothing was left to fill.
	By("filling the rest of the volume")
	filled := time.Now()
	output, err = utils.FillTmpfs(node, t.name, 0)
	Expect(err).NotTo(HaveOccurred(), "Failed to fill the rest of the volume")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)

	By("seeing the agent restarted once and Synced say WriteFailed")
	Eventually(func(g Gomega) {
		trace.look(g, t, node, begun)
		g.Expect(t.syncPod(g).restarts).To(Equal(before.restarts+1), "the agent was not restarted, or was restarted more than once")
		_, synced, reason, message, _ := t.syncedWithMessage(g)
		g.Expect(synced).To(Equal("False"))
		g.Expect(reason).To(Equal("WriteFailed"))
		g.Expect(message).To(ContainSubstring("writing to the volume failed, as when it is full"))
	}, fullStopBound).WithPolling(2 * time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "%s after the rest was filled: WriteFailed and one restart\n", since(filled))

	By("checking the exit code of the agent that stopped and what the two containers say")
	exitCode, err := utils.Run(exec.Command("kubectl", "get", "pod", t.syncPodName(), "-n", t.namespace,
		"-o", "jsonpath={.status.containerStatuses[0].lastState.terminated.exitCode}"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the pod of the sync")
	Expect(exitCode).To(Equal("3"), "the agent did not stop as one that lost the writer lock")
	previous, err := utils.Run(exec.Command("kubectl", "logs", t.syncPodName(), "-n", t.namespace, "--previous"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the log of the agent that stopped")
	_, _ = fmt.Fprintf(GinkgoWriter, "the log of the agent that stopped:\n%s\n", previous)
	// The agent says that the heartbeat of the lock could not be written, with the error of the volume, and not that another agent took it.
	Expect(previous).To(ContainSubstring("the heartbeat of the writer lock could not be written for a whole TTL"))
	Expect(previous).NotTo(ContainSubstring("lost to another agent"))
	Expect(previous).To(ContainSubstring("no space left on device"))
	current := t.agentLog(Default)
	_, _ = fmt.Fprintf(GinkgoWriter, "the log of the agent that runs:\n%s\n", current)
	Expect(current).To(MatchRegexp(`check=Writable outcome=Failed reason=WriteFailed .*no space left on device`))
	// The volume was not given up on in a loop: the agent that runs has not stopped again since it started, and waits for room.
	Expect(t.syncPod(Default).restarts).To(Equal(before.restarts + 1))

	By("freeing the volume")
	freed := time.Now()
	output, err = utils.FreeTmpfs(node, t.name)
	Expect(err).NotTo(HaveOccurred(), "Failed to free the volume")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)

	// Whole is the checksum of the file that was pushed, and not the remote's, which would agree with a half file that the agent had
	// committed in its place.
	By("seeing the file in the pod of the application whole, by its checksum")
	whole := fmt.Sprintf("%x  ./%s", sha256.Sum256(bytes.Repeat([]byte("x"), fullBigMiB<<20)), fullBigFile)
	Eventually(func(g Gomega) {
		trace.look(g, t, node, begun)
		g.Expect(t.appChecksums(g)).To(ContainElement(whole), "the big file in the pod of the application is not whole")
		g.Expect(t.appChecksums(g)).To(HaveLen(2), "the files of the pod of the application are not the seeded one and the big one")
	}, fullFreedBound).WithPolling(2 * time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "%s after the volume was freed: the file is whole\n", since(freed))

	By("writing a file in the pod of the application and seeing it on the remote")
	t.appWrite("after-free.txt", "written after the volume was freed")
	Eventually(func(g Gomega) {
		g.Expect(t.remoteFiles(g)).To(ContainElement("after-free.txt"))
	}, dodBound).Should(Succeed())

	By("seeing Synced say that the volume and the repository agree at the tip of the remote")
	Eventually(func(g Gomega) {
		trace.look(g, t, node, begun)
		t.expectSyncedAtTip(g, freed)
	}, dodReportBound).WithPolling(2 * time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "%s after the volume was freed: Synced is True at the tip\n", since(freed))
	Expect(t.remoteChecksums(Default)).To(ContainElement(whole), "the big file on the remote is not whole")
	Expect(t.remoteCommitsAdding(fullBigFile)).To(HaveLen(1), "the history of the remote adds %s more than once, or never", fullBigFile)
	_, _ = fmt.Fprintf(GinkgoWriter, "the log of the agent after the volume was freed:\n%s\n", t.agentLog(Default))
	t.expectRemoteFsck()
	t.expectRestarts(before, 1)
}

// fillBeforeObjects leaves the volume room for two blocks, which is room for the slot of the writer lock and for less than the objects
// of a commit, and pushes a commit from Git. The fetch cannot write its objects, and the agent says PullFailed, as for any write that
// the volume refuses, and not FetchFailed, which is for a host that cannot be reached, and removes the temporary object files that the
// fetch left. The objects that the fetch could write are kept, and before #257 the temporary ones were too: on kind the volume was full
// within five seconds, and the agent lost the lock and stopped 28 seconds after the fetch failed, as in the other spec. Synced says
// the failure only between two reads of the report by the operator, which is too short to wait for. So the volume is freed as soon as
// the log says it, and the agent, which still holds the lock, fetches the commit and is not restarted. This is what the agent does
// today, and the check before writing of #78 (M2) will give a full volume a reason of its own.
func fillBeforeObjects(t syncTarget, node string) {
	By("waiting for Synced to say that the volume and the repository agree at the tip")
	Eventually(func(g Gomega) {
		t.expectSyncedAtTip(g, time.Time{})
		g.Expect(t.syncPod(g).ready).To(BeTrue())
	}, fullFreedBound).WithPolling(2 * time.Second).Should(Succeed())
	before := t.syncPod(Default)

	By("filling the volume until it has room for two blocks")
	output, err := utils.FillTmpfs(node, t.name, fullSlotRoom)
	Expect(err).NotTo(HaveOccurred(), "Failed to fill the volume")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)

	By("pushing a commit from Git, whose objects do not fit")
	begun := time.Now()
	t.pushFromGit("objects.txt", "pushed while the volume had room for two blocks")

	By("seeing the fetch of the agent fail for want of room, which it says as PullFailed")
	var failed string
	Eventually(func(g Gomega) {
		failed = pullFailureSince(g, t, begun)
	}).WithPolling(time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "%s after the push: %s\n", since(begun), failed)
	Expect(failed).To(ContainSubstring("reason=PullFailed"))
	Expect(failed).To(ContainSubstring("because the volume cannot be written"))
	Expect(failed).To(ContainSubstring("No space left on device"))

	By("freeing the volume at once")
	freed := time.Now()
	output, err = utils.FreeTmpfs(node, t.name)
	Expect(err).NotTo(HaveOccurred(), "Failed to free the volume")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)

	By("seeing the commit in the pod of the application, and Synced say that the two agree at the tip")
	var trace fullTrace
	Eventually(func(g Gomega) {
		trace.look(g, t, node, begun)
		g.Expect(t.appFile(g, "objects.txt")).To(Equal("pushed while the volume had room for two blocks"))
	}, dodBound).WithPolling(2 * time.Second).Should(Succeed())
	Eventually(func(g Gomega) {
		trace.look(g, t, node, begun)
		t.expectSyncedAtTip(g, freed)
	}, dodReportBound).WithPolling(2 * time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "%s after the volume was freed: Synced is True at the tip\n", since(freed))
	t.expectRemoteFsck()
	t.expectRestarts(before, 0)
}

// setUpFullVolume mounts the tmpfs on the node, makes the volume on it and the claim for it, seeds the repository and applies the
// Treebind and the pod of the application, and returns the node. The claim has the name that failureManifest gives it, so that the
// Treebind names it as the other failure specs do.
func setUpFullVolume(t syncTarget) string {
	By("mounting a tmpfs of " + strconv.Itoa(fullVolumeMiB) + " MiB on the node")
	output, err := utils.Run(exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the node")
	node := strings.TrimSpace(output)
	output, err = utils.MountTmpfs(node, t.name, fullVolumeMiB)
	Expect(err).NotTo(HaveOccurred(), "Failed to mount a tmpfs on the node: the specs of the full volume need a node where the "+
		"privileged pod of hack/inject.sh can mount one")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)

	By("making the volume on it and the claim that is bound to it")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(fullClaimManifest(t, node))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the volume and the claim")
	Eventually(func(g Gomega) {
		phase, err := utils.Run(exec.Command("kubectl", "get", "pvc", t.name+"-data", "-n", t.namespace, "-o", "jsonpath={.status.phase}"))
		g.Expect(err).NotTo(HaveOccurred(), "Failed to read the claim")
		g.Expect(phase).To(Equal("Bound"))
	}).Should(Succeed())

	By("seeding a repository of its own on the remote")
	_, err = remoteShell("git init -q --bare --initial-branch=" + t.branch + " " + t.repository + " && git init -q --initial-branch=" +
		t.branch + " " + t.clone + " && cd " + t.clone + " && git config user.name dev && git config user.email dev@example.com && " +
		"echo hello > " + failureSeedFile + " && git add -A && git commit -q -m first && git push -q " + t.repository + " " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to seed the repository")

	By("applying its Treebind and the pod of its application")
	manifest := failureManifest(t)
	// failureManifest begins with a claim of the default class, which would be made next to the one that is bound to the tmpfs.
	Expect(manifest).To(HavePrefix("apiVersion: v1\nkind: PersistentVolumeClaim\n"))
	_, rest, found := strings.Cut(manifest, "\n---\n")
	Expect(found).To(BeTrue(), "the manifest of a failure spec has more than its claim")
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(rest)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the manifest")
	return node
}

// fullClaimManifest is the PersistentVolume on the tmpfs and the claim that names it. The volume is of type local, with the node that the
// tmpfs is on, so that the pods that use it run there, and it is kept when its claim goes, so that nothing deletes the directory.
func fullClaimManifest(t syncTarget, node string) string {
	return `apiVersion: v1
kind: PersistentVolume
metadata:
  name: ` + t.name + `
spec:
  capacity:
    storage: ` + strconv.Itoa(fullVolumeMiB) + `Mi
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ` + fullStorageClass + `
  volumeMode: Filesystem
  local:
    path: ` + utils.TmpfsDir(t.name) + `
  nodeAffinity:
    required:
      nodeSelectorTerms:
      - matchExpressions:
        - {key: kubernetes.io/hostname, operator: In, values: [` + node + `]}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ` + t.name + `-data
  namespace: ` + t.namespace + `
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ` + fullStorageClass + `
  volumeName: ` + t.name + `
  resources:
    requests:
      storage: ` + strconv.Itoa(fullVolumeMiB) + `Mi
`
}

// afterFullVolume frees the filler, whether the spec passed or not, after writing what tells why a spec that failed did.
func afterFullVolume(t syncTarget, node string) {
	if CurrentSpecReport().Failed() {
		output, err := utils.TmpfsStatus(node, t.name)
		_, _ = fmt.Fprintf(GinkgoWriter, "the volume: %s (%v)\n", output, err)
		diagnoseFailure(t)
		for _, args := range [][]string{
			{"describe", "pv", t.name},
			{"describe", "pvc", t.name + "-data", "-n", t.namespace},
		} {
			output, err := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(%v)\n", strings.Join(args, " "), output, err)
		}
	}
	output, err := utils.FreeTmpfs(node, t.name)
	_, _ = fmt.Fprintf(GinkgoWriter, "freeing the volume: %s (%v)\n", output, err)
}

// tearDownFullVolume removes what the Context made, in the order that lets each go: the Treebind, the pod of the application and the
// claim, then the volume, then the tmpfs, which is unmounted when nothing uses it any more.
func tearDownFullVolume(t syncTarget, node string) {
	tearDownFailure(t)
	By("removing the volume and unmounting the tmpfs")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "pv/"+t.name, "--ignore-not-found", "--timeout=3m"))
	if node == "" {
		return
	}
	output, err := utils.UnmountTmpfs(node, t.name)
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", output)
	Expect(err).NotTo(HaveOccurred(), "Failed to unmount the tmpfs of the volume")
}

// pushBigFromGit makes a commit with the big file in the clone that the seeding made, after taking what the repository has, and
// pushes it.
func (t syncTarget) pushBigFromGit() {
	_, err := remoteShell("cd " + t.clone + " && git pull -q " + t.repository + " " + t.branch + " && head -c " +
		strconv.Itoa(fullBigMiB<<20) + " /dev/zero | tr '\\0' x > " + fullBigFile + " && git add -A && " +
		"git commit -q -m 'from git: a big file' && git push -q " + t.repository + " " + t.branch)
	Expect(err).NotTo(HaveOccurred(), "Failed to push the big file from Git")
}

// pullFailedSinceLine is a line of the log of the agent in which a pull says that it failed.
var pullFailedSinceLine = regexp.MustCompile(`(?m)^time=\S+ level=\S+ msg=pull check=Pull outcome=Failed .*$`)

// pullFailureSince returns the line of the log of the agent in which a pull failed after the time, or fails.
func pullFailureSince(g Gomega, t syncTarget, from time.Time) string {
	line := pullFailedSinceLine.FindString(t.agentLogSince(g, from))
	g.Expect(line).NotTo(BeEmpty(), "no pull has failed since the push")
	return line
}

// fullTrace writes to the log of the spec what the Treebind, the pod and the volume say each time one of them changes, with the time
// since the first look, so that what the agent does on a full volume, and how long each step takes, is in the log of a run.
type fullTrace struct{ last string }

func (f *fullTrace) look(g Gomega, t syncTarget, node string, begun time.Time) {
	ready, synced, reason, _, _ := t.syncedWithMessage(g)
	state := t.syncPod(g)
	status, _ := utils.TmpfsStatus(node, t.name)
	available := "?"
	if match := regexp.MustCompile(`available=(\d+)`).FindStringSubmatch(status); match != nil {
		available = match[1]
	}
	file, _ := utils.Run(exec.Command("kubectl", "-n", t.namespace, "exec", t.appPod, "--", "sh", "-c",
		"stat -c %s "+t.appMount+"/"+fullBigFile+" 2>/dev/null || echo none"))
	reading := fmt.Sprintf("Ready=%s Synced=%s/%s restarts=%d podReady=%t available=%s %s=%s", ready, synced, reason, state.restarts,
		state.ready, available, fullBigFile, strings.TrimSpace(file))
	if reading != f.last {
		f.last = reading
		_, _ = fmt.Fprintf(GinkgoWriter, "%s: %s\n", since(begun), reading)
	}
}
