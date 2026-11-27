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

package maintain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

const (
	branchMain = "main"
	subPath    = "data"
	callRename = "rename"
)

// The refs that keep commits for a person, and that nothing else reaches: the recovery refs of the deletion guard (docs/adr/0026),
// the commits of the checkout that a resolution left (docs/adr/0034), and a ref that a person made by hand, as the page on
// recovering tells them to (docs/recovering-from-a-conflict-or-a-rejected-push.md).
var parkedRefs = []string{"refs/treebind/recovery/00000001", "refs/treebind/local/00000001", "refs/treebind/recovery/by-hand-20261127"}

// repository is a checkout of the agent with history, commits that only the parked refs keep, objects that nothing reaches, some
// packed and some loose, and loose objects for the maintenance to pack.
type repository struct {
	t *testing.T
	f *gittest.Fixture
	l layout.Layout
}

func newRepository(t *testing.T) *repository {
	t.Helper()
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{"a.txt": "a\n", "dir/b.txt": "b\n"})
	l := f.Layout(subPath)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	r := &repository{t: t, f: f, l: l}
	for i := range 20 {
		gittest.Write(t, l, fmt.Sprintf("dir/file-%02d.txt", i), strings.Repeat(fmt.Sprintf("line %d\n", i), 200))
		r.git("add", "-A")
		r.git("commit", "-q", "-m", fmt.Sprintf("commit %d", i))
	}
	tree := r.git("rev-parse", "HEAD^{tree}")
	for i, ref := range parkedRefs {
		// A commit of its own, with a blob of its own, that no branch and no reflog has: only the ref keeps it.
		blob := r.hash(fmt.Sprintf("parked %d\n", i))
		r.git("update-index", "--add", "--cacheinfo", "100644,"+blob+",parked.txt")
		parked := r.git("write-tree")
		r.git("read-tree", "HEAD")
		commit := r.git("commit-tree", parked, "-p", "HEAD", "-m", "parked "+ref)
		r.git("update-ref", ref, commit, "")
	}
	// A commit that was packed while a ref had it, and that nothing reaches since, and a blob that nothing ever reached.
	gone := r.git("commit-tree", tree, "-p", "HEAD", "-m", "packed and unreachable")
	r.git("update-ref", "refs/treebind/gone", gone, "")
	r.git("repack", "-d", "-n", "-q")
	r.git("update-ref", "-d", "refs/treebind/gone")
	r.hash("loose and unreachable\n")
	// Loose objects for the maintenance.
	for i := range 5 {
		gittest.Write(t, l, fmt.Sprintf("later-%d.txt", i), fmt.Sprintf("later %d\n", i))
		r.git("add", "-A")
		r.git("commit", "-q", "-m", fmt.Sprintf("later %d", i))
	}
	return r
}

// git runs plain Git on the checkout.
func (r *repository) git(args ...string) string {
	r.t.Helper()
	return r.f.InCheckout(r.l, args...)
}

// hash writes a blob of the content and returns its id.
func (r *repository) hash(content string) string {
	r.t.Helper()
	file := filepath.Join(gittest.TempDir(r.t), "blob")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return r.git("hash-object", "-w", file)
}

// objects lists every object in the repository, packed or loose, reachable or not.
func (r *repository) objects() []string {
	r.t.Helper()
	return strings.Fields(r.git("cat-file", "--batch-all-objects", "--batch-check=%(objectname)"))
}

// usable fails the test unless every object that was there before is there still, the history of every ref is whole, the commits
// of the parked refs are those they were, and the check of the Git directory that every command of the agent makes takes it.
func (r *repository) usable(before []string, parked map[string]string) {
	r.t.Helper()
	after := r.objects()
	for _, id := range before {
		if _, found := slices.BinarySearch(after, id); !found {
			r.t.Errorf("the object %s is gone", id)
		}
	}
	if _, err := r.f.PlainErr(r.l.WorkTree, []string{"GIT_DIR=" + r.l.GitDir, "GIT_WORK_TREE=" + r.l.WorkTree},
		"fsck", "--no-dangling", "--no-progress"); err != nil {
		r.t.Errorf("fsck: %v", err)
	}
	for ref, commit := range parked {
		if got := r.git("rev-parse", "--verify", ref); got != commit {
			r.t.Errorf("%s is at %s, want %s", ref, got, commit)
		}
		r.git("rev-list", "--objects", ref)
		r.git("cat-file", "-p", ref+":parked.txt")
	}
	if err := git.VerifyGitDir(r.l.GitDir, r.l.WorkTree); err != nil {
		r.t.Errorf("the check of the Git directory refuses it: %v", err)
	}
	for _, name := range []string{"packed-refs", "info/refs", "objects/info/packs"} {
		if gittest.Exists(filepath.Join(r.l.GitDir, name)) {
			r.t.Errorf("%s is in the Git directory", name)
		}
	}
	// The agent's own Runner runs a command on it.
	runner, err := git.New(r.l.RunnerOptions(r.f.GitOptions()))
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), "rev-list", "--objects", "--all"); err != nil {
		r.t.Errorf("the Runner of the agent cannot list the objects: %v", err)
	}
}

// parked is where the parked refs are.
func (r *repository) parked() map[string]string {
	r.t.Helper()
	parked := map[string]string{}
	for _, ref := range parkedRefs {
		parked[ref] = r.git("rev-parse", "--verify", ref)
	}
	return parked
}

// leftovers are the files that a Git that was stopped leaves among the objects.
func (r *repository) leftovers() []string {
	r.t.Helper()
	left, err := git.StaleTempObjects(r.l.GitDir, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return left
}

// maintainer is a Maintainer of the repository with the options changed.
func (r *repository) maintainer(change ...func(*Options)) *Maintainer {
	r.t.Helper()
	o := Options{Layout: r.l, Git: r.f.GitOptions(), LooseLimit: 2, PackLimit: 100}
	for _, c := range change {
		c(&o)
	}
	m, err := New(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func TestMaintenanceKeepsEveryObjectAndTheCommitsOfTheParkedRefs(t *testing.T) {
	r := newRepository(t)
	before, parked := r.objects(), r.parked()
	loose := countLoose(t, r.l.GitDir)

	got, err := r.maintainer().Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Packed || got.Before.Loose != loose || got.After.Loose >= loose {
		t.Errorf("the maintenance = %+v, want the %d loose objects packed", got, loose)
	}
	r.usable(before, parked)

	// Every pack into one, with what no ref reaches: nothing is dropped.
	got, err = r.maintainer(func(o *Options) { o.PackLimit = 2 }).Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Consolidated || got.After.Packs != 1 || got.After.Loose != 0 {
		t.Errorf("the maintenance = %+v, want every object in one pack", got)
	}
	r.usable(before, parked)
	if got.After.Bytes >= got.Before.Bytes {
		t.Errorf("the objects took %d bytes and take %d, want less", got.Before.Bytes, got.After.Bytes)
	}
}

// strace stops a real Git with SIGKILL at the system call that it is told, which is a kill at a known point of a repack, and
// not one at a moment that a test cannot choose. Without -f only the repack is traced, and with it pack-objects too, which is
// the first to make each call.
func straceGit(t *testing.T, follow bool, call string, nth int) string {
	t.Helper()
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skip("strace is not installed, and the kill of a repack at a chosen point needs it")
	}
	if out, err := exec.Command(strace, "-o", "/dev/null", "true").CombinedOutput(); err != nil {
		t.Skipf("strace cannot trace here: %v: %s", err, out)
	}
	real := gittest.Binary(t)
	flags := ""
	if follow {
		flags = "-f"
	}
	script := filepath.Join(gittest.TempDir(t), "git")
	body := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = repack ]; then
    exec '%s' %s -o /dev/null -e trace=%s -e inject=%s:signal=SIGKILL:when=%d '%s' "$@"
  fi
done
exec '%s' "$@"
`, strace, flags, call, call, nth, real, real)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestAMaintenanceThatIsKilledLeavesAUsableRepositoryAndTheNextPutsItRight(t *testing.T) {
	cases := []struct {
		name   string
		follow bool
		call   string
		nth    int
	}{
		{"pack-objects, as it moves its pack to the name of the repack", true, callRename, 1},
		{"pack-objects, as it moves the index", true, callRename, 2},
		{"the repack, as it moves the pack to its name", false, callRename, 1},
		{"the repack, as it moves the index to its name", false, callRename, 2},
		{"the repack, in the middle of removing the loose objects that it packed", false, "unlink", 5},
	}
	for _, full := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("full %v, %s", full, c.name), func(t *testing.T) {
				r := newRepository(t)
				before, parked := r.objects(), r.parked()
				killed := r.maintainer(func(o *Options) {
					o.Git.Git = straceGit(t, c.follow, c.call, c.nth)
					if full {
						o.PackLimit = 1
					}
				})

				// As the agent is killed with its Git, nothing is put right after the repack: what it left is what the next
				// start of the agent finds.
				if _, err := killed.repack(context.Background(), full); err == nil {
					t.Fatal("the repack was not killed")
				}
				left := r.leftovers()
				if c.call == callRename && len(left) == 0 {
					t.Fatal("the killed repack left nothing among the packs: the test shows nothing")
				}
				t.Logf("left: %v", left)
				r.usable(before, parked)

				got, err := r.maintainer(func(o *Options) {
					if full {
						o.PackLimit = 1
					}
				}).Maintain(context.Background())
				if err != nil {
					t.Fatalf("the maintenance after the kill: %v", err)
				}
				if left := r.leftovers(); len(left) != 0 {
					t.Errorf("after the next maintenance %v are left", left)
				}
				if got.After.Loose != 0 && full {
					t.Errorf("after the next maintenance %d objects are loose", got.After.Loose)
				}
				r.usable(before, parked)
			})
		}
	}
}

// The maintenance that the agent runs cleans up after a repack that failed under it, as after a fetch that the volume refused.
func TestAMaintenanceWhoseRepackFailsRemovesWhatTheRepackLeft(t *testing.T) {
	r := newRepository(t)
	before, parked := r.objects(), r.parked()
	m := r.maintainer(func(o *Options) { o.Git.Git = straceGit(t, false, callRename, 1) })

	_, err := m.Maintain(context.Background())

	if err == nil {
		t.Fatal("the maintenance passed, want the killed repack to fail it")
	}
	if left := r.leftovers(); len(left) != 0 {
		t.Errorf("%v are left after the maintenance failed", left)
	}
	r.usable(before, parked)
}

func TestMaintenanceRemovesWhatAKilledGitLeftAmongTheObjectsFirst(t *testing.T) {
	r := newRepository(t)
	before, parked := r.objects(), r.parked()
	left := []string{"objects/pack/.tmp-77-pack-0123456789abcdef0123456789abcdef01234567.pack", "objects/pack/tmp_pack_Ab12Cd"}
	for _, name := range left {
		if err := os.WriteFile(filepath.Join(r.l.GitDir, name), []byte("half a pack"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.maintainer().Maintain(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Removed, slices.Sorted(slices.Values(left))) {
		t.Errorf("removed %v, want %v", got.Removed, left)
	}
	if !strings.Contains(got.String(), left[0]) {
		t.Errorf("the note %q does not name what was removed", got.String())
	}
	r.usable(before, parked)
}

func TestMaintenanceWithoutTheRoomForItWritesNothingAndSaysHowMuchItNeeds(t *testing.T) {
	r := newRepository(t)
	loose := countLoose(t, r.l.GitDir)
	m := r.maintainer(func(o *Options) { o.Free = func(string) (int64, error) { return 1000, nil } })

	got, err := m.Maintain(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != NoRoom || got.Need <= 1000 || got.Free != 1000 {
		t.Errorf("the maintenance = %+v, want NoRoom with what it needs", got)
	}
	if after := countLoose(t, r.l.GitDir); after != loose {
		t.Errorf("%d loose objects, want the %d that were there", after, loose)
	}
	if note := got.String(); !strings.Contains(note, "needs") {
		t.Errorf("the note %q does not say what is needed", note)
	}
}

func TestMaintenanceWithoutTheRoomToPutEveryPackInOnePacksTheLooseObjects(t *testing.T) {
	r := newRepository(t)
	s, err := survey(r.l.GitDir)
	if err != nil {
		t.Fatal(err)
	}
	m := r.maintainer(func(o *Options) {
		o.PackLimit = 1
		// Room for the loose objects and not for every pack again.
		o.Free = func(string) (int64, error) {
			return s.looseSize + int64(s.Loose)*indexPerObject + fixedPerPack + DefaultReserve + s.packSize - 1, nil
		}
	})

	got, err := m.Maintain(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Packed || got.After.Packs != s.Packs+1 {
		t.Errorf("the maintenance = %+v, want the loose objects packed", got)
	}
}

func TestMaintenanceWritesNothingWhenTheFenceFails(t *testing.T) {
	r := newRepository(t)
	loose := countLoose(t, r.l.GitDir)
	lost := errors.New("the lock is lost")
	m := r.maintainer(func(o *Options) { o.Fence = func(context.Context) error { return lost } })

	_, err := m.Maintain(context.Background())

	if !errors.Is(err, lost) {
		t.Errorf("the maintenance = %v, want the error of the fence", err)
	}
	if after := countLoose(t, r.l.GitDir); after != loose {
		t.Errorf("%d loose objects, want the %d that were there", after, loose)
	}
}

func TestMaintenanceIsDueAtTheLimitsThatGitHasForGarbageCollection(t *testing.T) {
	gitDir := gittest.TempDir(t)
	l := layout.Layout{GitDir: gitDir}
	m := &Maintainer{o: Options{Layout: l, LooseLimit: DefaultLooseLimit, PackLimit: DefaultPackLimit}}
	plant := func(name string) {
		t.Helper()
		path := filepath.Join(gitDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	due := func() bool {
		t.Helper()
		got, err := m.Due()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if due() {
		t.Error("an empty Git directory is due")
	}
	// Git samples the directory objects/17, and is due when it has more than its share of gc.auto, 6700 / 256 rounded up.
	for i := range 27 {
		plant(fmt.Sprintf("objects/17/%038x", i))
	}
	plant("objects/17/tmp_obj_x")
	if due() {
		t.Error("27 loose objects in objects/17 are due, want 28")
	}
	plant(fmt.Sprintf("objects/17/%038x", 27))
	if !due() {
		t.Error("28 loose objects in objects/17 are not due")
	}

	m.o.Layout.GitDir = gittest.TempDir(t)
	gitDir = m.o.Layout.GitDir
	for i := range 49 {
		plant(fmt.Sprintf("objects/pack/pack-%040x.idx", i))
	}
	// A pack that is kept is not counted, as Git does not.
	plant(fmt.Sprintf("objects/pack/pack-%040x.idx", 49))
	plant(fmt.Sprintf("objects/pack/pack-%040x.keep", 49))
	if due() {
		t.Error("49 packs are due, want 50")
	}
	plant(fmt.Sprintf("objects/pack/pack-%040x.idx", 50))
	if !due() {
		t.Error("50 packs are not due")
	}
}

// An out-of-memory kill takes the agent with its Git, and a repack that is killed for its memory would be again at each start. A
// maintenance that was cut off is not tried again for a day.
func TestAMaintenanceThatWasCutOffIsNotTriedAgainForADay(t *testing.T) {
	r := newRepository(t)
	now := time.Unix(1_800_000_000, 0)
	m := r.maintainer(func(o *Options) { o.Now = func() time.Time { return now } })
	if err := os.WriteFile(filepath.Join(r.l.StateDir, markerName), []byte(strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := countLoose(t, r.l.GitDir)

	got, err := m.Maintain(context.Background())

	if err != nil || got.Outcome != Held {
		t.Fatalf("the maintenance = %+v, %v, want it held", got, err)
	}
	if after := countLoose(t, r.l.GitDir); after != loose {
		t.Errorf("%d loose objects, want the %d that were there", after, loose)
	}
	if note := got.String(); !strings.Contains(note, "cut off") {
		t.Errorf("the note %q does not say why", note)
	}

	now = now.Add(24 * time.Hour)
	if got, err := m.Maintain(context.Background()); err != nil || got.Outcome != Packed {
		t.Fatalf("the maintenance a day later = %+v, %v, want the objects packed", got, err)
	}
	if gittest.Exists(filepath.Join(r.l.StateDir, markerName)) {
		t.Error("the marker is still there after a maintenance that ended")
	}
}

// countLoose counts the loose objects.
func countLoose(t *testing.T, gitDir string) int {
	t.Helper()
	var n int
	dirs, err := os.ReadDir(filepath.Join(gitDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if len(dir.Name()) != 2 || !dir.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(gitDir, "objects", dir.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if len(entry.Name()) == 38 {
				n++
			}
		}
	}
	return n
}
