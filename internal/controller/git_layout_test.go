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
	"path"
	"reflect"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// The Git directory is beside the synced directory on the claim (docs/adr/0016). That
// takes four things that no test of one side alone can show: the operator passes the
// sub path on, the agent reads it and works out the same directory, the claim is
// mounted whole so that the state directory is there, and the API server and the agent
// agree on which sub paths exist.

// The sub paths that more than one test uses.
const (
	subPathOne       = "grafana"
	subPathNested    = "team-a/dashboards"
	subPathLookalike = ".treebindx"
	subPathRelease   = "release-1.2"
	subPathState     = ".treebind"
	subPathStateGit  = ".treebind/git"
)

func agentEnvironmentOf(container corev1.Container) map[string]string {
	env := map[string]string{}
	for _, variable := range container.Env {
		env[variable.Name] = variable.Value
		if variable.ValueFrom != nil {
			env[variable.Name] = testNamespace // metadata.namespace, the only field the pod reads
		}
	}
	return env
}

func TestAgentEnvironmentWithASubPath(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Namespace = testNamespace
	treebind.Spec.Target.MountPath = testCustomPath
	treebind.Spec.Target.SubPath = subPathNested

	container, _ := agentContainer(t, treebind)

	names := make([]string, 0, len(container.Env))
	for _, variable := range container.Env {
		names = append(names, variable.Name)
	}
	want := []string{
		"TREEBIND_NAME", "TREEBIND_NAMESPACE", "TREEBIND_MOUNT_PATH", "TREEBIND_SUB_PATH",
		"TREEBIND_REPO_URL", "TREEBIND_REPO_BRANCH", "TREEBIND_STATUS_ADDR", "TREEBIND_STATUS_ISSUER", "TREEBIND_STATUS_AUDIENCE",
		"TREEBIND_STATUS_SUBJECT", "TREEBIND_STATUS_KEYS_FILE",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("variables = %v\nwant        %v", names, want)
	}
	if got := agentEnvironmentOf(container)[agentconfig.EnvSubPath]; got != subPathNested {
		t.Errorf("%s = %q, want the sub path as the spec has it", agentconfig.EnvSubPath, got)
	}
}

func TestNoSubPathVariableWithoutASubPath(t *testing.T) {
	container, _ := agentContainer(t, newTreebind(testTreebindName))

	for _, variable := range container.Env {
		if variable.Name == agentconfig.EnvSubPath {
			t.Errorf("%s is set to %q: a pod template that had no sub path must not change, "+
				"or every such sync pod is replaced when the operator is upgraded", variable.Name, variable.Value)
		}
	}
}

func TestTheAgentWorksOutTheDirectoryTheSpecNames(t *testing.T) {
	tests := []struct {
		name, mountPath, subPath string
		wantWorkTree             string
		wantGitDirInWorkTree     bool
	}{
		{"a sub path", "", subPathOne, defaultMountPath + "/grafana", false},
		{"a nested sub path at a custom mount path", testCustomPath, subPathNested, testCustomPath + "/team-a/dashboards", false},
		{"a mount path that is not clean", "/data//grafana/", subPathOne, "/data/grafana/grafana", false},
		{"no sub path", "", "", defaultMountPath, true},
		{"no sub path at a custom mount path", testCustomPath, "", testCustomPath, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Namespace = testNamespace
			treebind.Spec.Target.MountPath = tt.mountPath
			treebind.Spec.Target.SubPath = tt.subPath
			container, _ := agentContainer(t, treebind)
			env := agentEnvironmentOf(container)

			config, err := agentconfig.FromEnv(func(name string) string { return env[name] })
			if err != nil {
				t.Fatalf("the agent refuses the environment of its own pod: %v", err)
			}
			got, err := layout.New(config.MountPath, config.SubPath)
			if err != nil {
				t.Fatalf("the agent cannot work out its layout from its own pod: %v", err)
			}

			if got.WorkTree != tt.wantWorkTree || got.GitDirInWorkTree != tt.wantGitDirInWorkTree {
				t.Errorf("work tree = %s (Git directory in it: %v), want %s (%v)",
					got.WorkTree, got.GitDirInWorkTree, tt.wantWorkTree, tt.wantGitDirInWorkTree)
			}
			if got.GitDir != path.Join(config.MountPath, subPathState, "git") {
				t.Errorf("Git directory = %s, want it in the state directory at the root of the claim", got.GitDir)
			}
		})
	}
}

// The layout needs the state directory at the root of the claim, so the claim is
// mounted whole in the sync pod, whatever the sub path. Mounting only the sub path in
// the sync pod would hide it and leave the Git directory nowhere.
func TestTheSyncPodMountsTheWholeClaimAtTheMountPathTheAgentIsTold(t *testing.T) {
	for _, subPath := range []string{"", subPathOne, subPathNested} {
		t.Run("sub path "+subPath, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Namespace = testNamespace
			treebind.Spec.Target.MountPath = testCustomPath
			treebind.Spec.Target.SubPath = subPath
			container, _ := agentContainer(t, treebind)

			var mounts []corev1.VolumeMount
			for _, mount := range container.VolumeMounts {
				if mount.Name == dataVolumeName {
					mounts = append(mounts, mount)
				}
			}
			if len(mounts) != 1 {
				t.Fatalf("the claim is mounted %d times, want once: %+v", len(mounts), mounts)
			}
			if mounts[0].SubPath != "" || mounts[0].SubPathExpr != "" {
				t.Errorf("the claim is mounted with the sub path %q: the state directory at its root would be out of reach",
					mounts[0].SubPath+mounts[0].SubPathExpr)
			}
			if mounts[0].MountPath != agentconfigMountPath(t, container) {
				t.Errorf("the claim is mounted at %s, the agent is told %s", mounts[0].MountPath, agentconfigMountPath(t, container))
			}
			if mounts[0].ReadOnly {
				t.Error("the claim is mounted read-only, and the agent writes its state to it")
			}
		})
	}
}

func agentconfigMountPath(t *testing.T, container corev1.Container) string {
	t.Helper()
	env := agentEnvironmentOf(container)
	config, err := agentconfig.FromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	return config.MountPath
}

// subPathCandidates are values for target.subPath that sit on both sides of every rule:
// the API server (CEL and the length limit) and the agent (CheckSubPath) have to agree
// on each of them.
func subPathCandidates() []string {
	long := strings.Repeat("a", agentconfig.MaxSubPathLength)
	return []string{
		"", subPathOne, subPathNested, "a/b/c/d", subPathRelease, ".config", ".github/dashboards",
		subPathLookalike, ".treebind-old", "a/.treebind", "a/b/.treebind", ".treebind2/git",
		"my dashboards", " a", "a ", " a b ", "é/ü", "日本語", strings.Repeat("é", agentconfig.MaxSubPathLength),
		long, long + "a", strings.Repeat("é", agentconfig.MaxSubPathLength+1),
		subPathState, subPathStateGit, ".treebind/", ".treebind/a/b", "./.treebind",
		"/data", "/", ".", "..", "../x", "a/..", "a/../b", "a/./b", "./a", "a//b", "//a", "a/",
		`a\b`, `\a`, ".git", "a/.git", ".git/hooks", "a/.git/b", ".gitx", ".github", "git",
		"a\x00b", "a\nb", "a\tb", "a\rb", "\x7f", "a\u00a0b", "a\u2028b",
	}
}

var _ = Describe("target.subPath", func() {
	var counter int

	// created returns whether the API server accepted a Treebind with the sub path,
	// and the error when it did not.
	created := func(ctx context.Context, subPath string) (bool, error) {
		GinkgoHelper()
		counter++
		treebind := newTreebind(fmt.Sprintf("subpath-%d", counter))
		treebind.Spec.Target.SubPath = subPath
		err := k8sClient.Create(ctx, treebind)
		if err == nil {
			Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
		}
		return err == nil, err
	}

	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for _, subPath := range subPathCandidates() {
			ok, err := created(ctx, subPath)
			agentOK := agentconfig.CheckSubPath(subPath) == nil
			if ok != agentOK {
				Fail(fmt.Sprintf("sub path %q: the API server accepts it = %v (%v), the agent accepts it = %v: "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					subPath, ok, err, agentOK))
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

	It("refuses the directory of the agent's own state, and says why", func() {
		ctx := context.Background()
		for _, subPath := range []string{subPathState, subPathStateGit, ".treebind/a/b"} {
			ok, err := created(ctx, subPath)

			Expect(ok).To(BeFalse(), subPath)
			Expect(err).To(MatchError(ContainSubstring("subPath must not start with '.treebind'")), subPath)
		}
	})

	It("accepts a name that only looks like it, or that has it below the first segment", func() {
		ctx := context.Background()
		for _, subPath := range []string{subPathLookalike, ".treebind-old", "a/.treebind"} {
			ok, err := created(ctx, subPath)

			Expect(ok).To(BeTrue(), "%s: %v", subPath, err)
		}
	})
})
