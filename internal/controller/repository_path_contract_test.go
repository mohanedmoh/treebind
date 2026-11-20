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

package controller

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// repositoryPathCandidates are values for repository.path that sit on both sides of every rule of the API server (CEL and the
// length limit) and of the agent (CheckRepoPath), which have to agree on each of them: the candidates of the sub path, which has
// the same rules but one, and the names that the one is about.
func repositoryPathCandidates() []string {
	return append(subPathCandidates(), ".treebind/cfg", "a/.treebind/b", ".treebind", ".treebind/", ".TREEBIND/x",
		strings.Repeat("a/", 512), strings.Repeat("a/", 511)+"a")
}

var _ = Describe("repository.path", func() {
	var counter int

	created := func(ctx context.Context, path string) (bool, error) {
		GinkgoHelper()
		counter++
		treebind := newTreebind(fmt.Sprintf("repopath-%d", counter))
		treebind.Spec.Repository.Path = path
		err := k8sClient.Create(ctx, treebind)
		if err == nil {
			Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
		}
		return err == nil, err
	}

	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for _, path := range repositoryPathCandidates() {
			ok, err := created(ctx, path)
			agentOK := agentconfig.CheckRepoPath(path) == nil
			if ok != agentOK {
				Fail(fmt.Sprintf("path %q: the API server accepts it = %v (%v), the agent accepts it = %v: "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					path, ok, err, agentOK))
			}
			if ok {
				accepted++
			} else {
				refused++
			}
		}
		// The list has to hold both, or the check above says nothing.
		Expect(accepted).To(BeNumerically(">", 10))
		Expect(refused).To(BeNumerically(">", 10))
	})
})
