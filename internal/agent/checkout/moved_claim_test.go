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

package checkout_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// The claim is mounted at /work for every Treebind since spec.target.mountPath was taken out (docs/adr/0041). The Git
// directory on the claim holds the path of the synced directory as it was mounted when the checkout was made, in core.worktree,
// and the agent refuses a path that is not the one it writes. So a checkout that was made where a Treebind set another
// mountPath is not opened until that one setting names the new path, which is the step that the migration has (#295).
func TestACheckoutOfAClaimThatWasMountedAtAnotherPathIsRefusedUntilCoreWorktreeNamesTheNewOne(t *testing.T) {
	for _, subPath := range []string{"", "dashboards"} {
		t.Run("sub path "+subPath, func(t *testing.T) {
			f := gittest.New(t)
			f.Publish("main", map[string]string{"a.json": "a\n"})
			before := f.Layout(subPath)
			if _, err := checkout.Open(context.Background(), f.CheckoutOptions(before, "main")); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(gittest.TempDir(t), "work")
			if err := os.Rename(f.Claim, moved); err != nil {
				t.Fatal(err)
			}
			after, err := layout.New(moved, subPath)
			if err != nil {
				t.Fatal(err)
			}

			_, err = checkout.Open(context.Background(), f.CheckoutOptions(after, "main"))

			var refused *checkout.Error
			if !errors.As(err, &refused) || refused.Reason != checkout.ReasonGitDirUnsafe || !strings.Contains(refused.Message, "core.worktree") {
				t.Fatalf("checkout = %v, want it refused with GitDirUnsafe for core.worktree", err)
			}

			config := filepath.Join(after.GitDir, "config")
			if out, err := exec.Command("git", "config", "--file", config, "core.worktree", after.WorkTree).CombinedOutput(); err != nil {
				t.Fatalf("git config: %v: %s", err, out)
			}
			got, err := checkout.Open(context.Background(), f.CheckoutOptions(after, "main"))

			if err != nil || got.Outcome != checkout.Reopened {
				t.Fatalf("checkout = %+v, %v, want the checkout that was there, reopened", got, err)
			}
			if data := gittest.Read(t, after, "a.json"); data != "a\n" {
				t.Errorf("a.json = %q, want it as it was", data)
			}
		})
	}
}
