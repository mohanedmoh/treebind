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
	"testing"

	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The directory of the repository that is the synced directory (docs/adr/0036) is passed to the agent as TREEBIND_REPO_PATH,
// when the spec sets it and not otherwise, so that no pod template that never had it changes.

const (
	repoPathDashboards = "grafana/dashboards"
	repoPathTwoByte    = "é/ü"
)

// repositoryPaths are values of spec.repository.path that the CRD takes, with the characters that the kubelet would rewrite in the
// value of an environment variable and the ones that it leaves as they are.
var repositoryPaths = []string{
	repoPathDashboards, ".treebind/cfg", "a b", " a ", repoPathTwoByte, "日本語/x", "cost$5", "$(TREEBIND_NAME)/x", "$$/a", "$", "a$", "x/$(NOPE)",
	"$(TREEBIND_NAMESPACE)$(TREEBIND_POD_NAME)", "a/$", "release-1.2/.github",
}

func TestTheAgentIsToldNoRepositoryPathWhenTheSpecSetsNone(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Namespace = testNamespace

	container, _ := agentContainer(t, treebind)

	if _, there := agentEnvironmentOf(container)[agentconfig.EnvRepoPath]; there {
		t.Errorf("%s is in the pod of a Treebind that sets no path, so its sync pod would be replaced for nothing", agentconfig.EnvRepoPath)
	}
}

func TestTheAgentReadsTheRepositoryPathAsTheSpecWritesItAfterTheKubelet(t *testing.T) {
	for _, path := range repositoryPaths {
		treebind := newTreebind(testTreebindName)
		treebind.Namespace = testNamespace
		treebind.Spec.Repository.Path = path
		container, _ := agentContainer(t, treebind)

		env := environmentAfterTheKubelet(container)
		config, err := agentconfig.FromEnv(func(name string) string { return env[name] })

		if err != nil {
			t.Errorf("the agent refuses the environment of its own pod for the path %q: %v", path, err)
			continue
		}
		if config.RepoPath != path {
			t.Errorf("the agent reads the path %q as %q", path, config.RepoPath)
		}
	}
}

func TestTemplateHashFollowsTheRepositoryPath(t *testing.T) {
	states := []string{"", repoPathDashboards, "grafana/alerts", ""}
	hashes := make([]string, len(states))
	for i, state := range states {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Repository.Path = state
		hashes[i] = mustDesiredStatefulSet(t, treebind, testImage).Annotations[templateHashAnnotation]
		if i > 0 && hashes[i] == hashes[i-1] {
			t.Errorf("going from %q to %q leaves the hash as it was, so the pod is not replaced and keeps the old directory", states[i-1], state)
		}
	}
	if hashes[0] != hashes[len(hashes)-1] {
		t.Errorf("a Treebind whose path is taken away has the hash %s, and %s before it had one", hashes[len(hashes)-1], hashes[0])
	}
}
