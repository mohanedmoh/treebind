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

package git

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	cmdRepack = "repack"
	hashA     = "0123456789abcdef0123456789abcdef01234567"
	hashB     = "89abcdef0123456789abcdef0123456789abcdef"
	hashC     = "fedcba9876543210fedcba9876543210fedcba98"
)

// A repack that is killed after pack-objects has written the pack, and before the repack has moved it to its name, leaves it
// under the name of the repack's process, .tmp-<pid>-pack-<hash>, and a repack that is killed between moving the .pack and the
// .idx leaves a pack without its index, which Git does not read (tried with a real repack on Git 2.34.1, docs/adr/0043). Neither
// is removed by the next repack, and the first can be as large as the repository.
func TestRemoveStaleTempObjectsRemovesWhatAKilledRepackLeavesAmongThePacks(t *testing.T) {
	gitDir := tempDir(t)
	temporary := []string{
		"objects/pack/.tmp-4242-pack-" + hashA + ".pack", "objects/pack/.tmp-4242-pack-" + hashA + ".idx",
		"objects/pack/.tmp-4242-pack-" + hashA + ".rev",
		// A pack whose index was not moved to its name, with the reverse index that a newer Git moves before the index.
		"objects/pack/pack-" + hashB + ".pack", "objects/pack/pack-" + hashB + ".rev", "objects/pack/.tmp-4242-pack-" + hashB + ".idx",
	}
	keep := []string{
		// A whole pack, with what Git keeps beside it.
		"objects/pack/pack-" + hashC + ".pack", "objects/pack/pack-" + hashC + ".idx", "objects/pack/pack-" + hashC + ".rev",
		"objects/pack/pack-" + hashC + ".keep",
		// A .keep and a .promisor say something of a pack whether or not it is there, and are not the agent's to remove.
		"objects/pack/pack-" + hashB + ".keep", "objects/pack/pack-" + hashB + ".promisor",
		// Names of another kind.
		"objects/pack/pack-" + hashB + ".pack.old", "objects/pack/pack-short.pack", "objects/pack/multi-pack-index",
		"objects/pack/tmp-4242-pack-" + hashA + ".pack", "objects/.tmp-4242-pack-" + hashA + ".pack",
		"objects/ab/.tmp-4242-pack-" + hashA + ".pack",
	}
	for _, name := range append(slices.Clone(temporary), keep...) {
		plant(t, gitDir, name)
	}

	listed, err := StaleTempObjects(gitDir, 0)
	if want := slices.Sorted(slices.Values(temporary)); err != nil || !slices.Equal(listed, want) {
		t.Fatalf("StaleTempObjects = %v, %v, want %v", listed, err, want)
	}
	got, err := RemoveStaleTempObjects(gitDir, 0)

	if err != nil || !slices.Equal(got, listed) {
		t.Fatalf("removed %v, %v, want %v", got, err, listed)
	}
	for _, name := range temporary {
		if exists(filepath.Join(gitDir, name)) {
			t.Errorf("%s is still there", name)
		}
	}
	for _, name := range keep {
		if !exists(filepath.Join(gitDir, name)) {
			t.Errorf("%s was removed", name)
		}
	}
}

// A pack without its index that is a link, or a directory, is not one that Git wrote, and is left.
func TestRemoveStaleTempObjectsRemovesNoLinkOrDirectoryAmongThePacks(t *testing.T) {
	gitDir := tempDir(t)
	outside := plant(t, tempDir(t), "pack")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects", "pack", "pack-"+hashA+".pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(gitDir, "objects", "pack", "pack-"+hashB+".pack"))
	mustSymlink(t, outside, filepath.Join(gitDir, "objects", "pack", ".tmp-1-pack-"+hashB+".pack"))

	got, err := RemoveStaleTempObjects(gitDir, 0)

	if err != nil || len(got) != 0 {
		t.Errorf("removed %v, %v, want nothing", got, err)
	}
	if !exists(outside) {
		t.Error("the file that a link points to was removed")
	}
}

// Without -n a repack runs update-server-info, which writes info/refs, and the check of the Git directory refuses a directory
// whose info is not empty (docs/adr/0019): a repack of the agent would make the agent refuse its own Git directory. The Runner
// runs a repack only with -n.
func TestTheRunnerRunsARepackOnlyWithMinusN(t *testing.T) {
	r := newRepo(t, outsideWork)
	if err := os.RemoveAll(filepath.Join(r.gitDir, "info")); err != nil {
		t.Fatal(err)
	}
	runner := r.runner()

	for _, args := range [][]string{{cmdRepack}, {cmdRepack, "-d", "-q"}, {cmdRepack, "-a", "-d"}, {cmdRepack, "--", "-n"}} {
		_, err := r.run(runner, args...)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("git %v = %v, want it refused", args, err)
		}
	}
	if exists(filepath.Join(r.gitDir, "info")) {
		t.Fatal("a refused repack wrote info")
	}
	for _, args := range [][]string{{cmdRepack, "-d", "-n", "-q"}, {cmdRepack, "-a", "-d", "-n", "-q", "--keep-unreachable"}} {
		if _, err := r.run(runner, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		for _, written := range []string{"info", "objects/info/packs"} {
			if exists(filepath.Join(r.gitDir, written)) {
				t.Errorf("git %v wrote %s", args, written)
			}
		}
	}

	// The trap springs with plain Git.
	r.plain(cmdRepack, "-d", "-q")
	if !exists(filepath.Join(r.gitDir, "info", "refs")) {
		t.Error("a plain repack without -n wrote no info/refs: the test shows nothing")
	}
}
