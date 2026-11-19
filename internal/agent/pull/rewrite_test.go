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

package pull

import (
	"context"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/push"
)

// A host that gives back what it was sent as another commit with the same files made a loop of the agent with followLocal:
// the checkout and the repository each had a commit that the other lacked, the merge that the policy made was pushed, the
// host rewrote it, and the next pull found the same again, at every interval, for as long as the agent ran (R-18).
func TestAHostThatRewritesWhatItIsSentDoesNotMakeAResolutionAtEveryPull(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			w.f.RewriteWhatIsPushed(false)
			pusher, err := push.New(push.Options{Layout: w.l, URL: w.f.URL, Branch: branchMain, Git: w.f.GitOptions(), Timeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			w.local(t, map[string]string{fileA: mine})
			if _, err := pusher.Push(context.Background()); err != nil {
				t.Fatal(err)
			}

			resolutions := 0
			for pass := 1; pass <= 6; pass++ {
				got, err := w.pull(t)
				if err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				if got.Outcome == Resolved {
					resolutions++
				}
				if pushed, err := pusher.Push(context.Background()); err != nil || pushed.Outcome != push.NothingToPush {
					t.Fatalf("pass %d: push = %+v, %v, want nothing to push: the checkout is the tip of the repository", pass, pushed, err)
				}
			}

			if resolutions != 1 {
				t.Errorf("the pulls resolved %d times in 6 passes, want once: the repository holds the files of the checkout", resolutions)
			}
		})
	}
}
