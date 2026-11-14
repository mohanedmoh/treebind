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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The agent clones the repository the Treebind names (docs/adr/0018). That takes three
// things that no test of one side alone can show: the operator puts the address, the
// branch and the files of the credentials in the pod, the agent accepts what the operator
// puts there, and the API server and the agent agree on which branches and addresses exist.

// The variables that the repository's settings arrive in.
const (
	envRepoURL        = agentconfig.EnvRepoURL
	envRepoBranch     = agentconfig.EnvRepoBranch
	envSSHKeyFile     = agentconfig.EnvSSHKeyFile
	envKnownHostsFile = agentconfig.EnvKnownHostsFile
)

func agentConfigOf(t *testing.T, treebind *treebindv1alpha1.Treebind) (agentconfig.Config, map[string]string, error) {
	t.Helper()
	treebind.Namespace = testNamespace
	container, _ := agentContainer(t, treebind)
	env := agentEnvironmentOf(container)
	config, err := agentconfig.FromEnv(func(name string) string { return env[name] })
	return config, env, err
}

func TestTheAgentIsToldTheRepositoryAndTheBranch(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Repository.URL = "https://git.example.com/acme/dashboards.git"
	treebind.Spec.Repository.Branch = branchReleaseSlash

	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	if config.RepoURL != treebind.Spec.Repository.URL || config.RepoBranch != branchReleaseSlash {
		t.Errorf("the agent reads %q on %q, want the repository and the branch of the spec", config.RepoURL, config.RepoBranch)
	}
	for _, name := range []string{envSSHKeyFile, envKnownHostsFile} {
		if value, set := env[name]; set {
			t.Errorf("%s is %q for a Treebind with no credentials", name, value)
		}
	}
}

func TestNoBranchVariableWithoutABranch(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Repository.Branch = ""

	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatal(err)
	}
	if value, set := env[envRepoBranch]; set {
		t.Errorf("%s is %q: an empty branch is the default branch, which the agent finds out", envRepoBranch, value)
	}
	if config.RepoBranch != "" {
		t.Errorf("the agent follows %q, want the default branch", config.RepoBranch)
	}
}

func TestTheAgentIsToldTheFilesOfTheCredentialsAndNeverTheirContent(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Repository.URL = sshRepositoryURL
	// Keys that are not the ones every other test uses, so that a name that is made up
	// cannot pass for the one the spec has.
	treebind.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
		SSHKeySecretRef:        &treebindv1alpha1.KeySelector{Name: testSSHKeyName, Key: "deploy_key"},
		KnownHostsConfigMapRef: &treebindv1alpha1.KeySelector{Name: testHostsName, Key: "hosts.pub"},
	}

	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	container, _ := agentContainer(t, treebind)
	for _, file := range []struct {
		env, volume, key string
		got              string
	}{
		{envSSHKeyFile, sshVolumeName, "deploy_key", config.SSHKeyFile},
		{envKnownHostsFile, knownHostsVolumeName, "hosts.pub", config.KnownHostsFile},
	} {
		// The file the agent is told of is a file that the pod has: the volume is mounted
		// where the path says, and has the key under that name.
		var mountPath string
		for _, mount := range container.VolumeMounts {
			if mount.Name == file.volume {
				mountPath = mount.MountPath
			}
		}
		if want := path.Join(mountPath, file.key); mountPath == "" || file.got != want || env[file.env] != want {
			t.Errorf("%s = %q, want %q, the key %s of the volume %s", file.env, file.got, want, file.key, file.volume)
		}
	}
	for _, variable := range container.Env {
		if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
			t.Errorf("%s is read from a Secret: a credential does not go through the environment (NFR-3)", variable.Name)
		}
	}
}

func TestTheAgentAcceptsTheRepositoriesTheOperatorCanPutInThePod(t *testing.T) {
	tests := []struct {
		name  string
		build func(*treebindv1alpha1.Treebind)
	}{
		{"an https repository", func(tb *treebindv1alpha1.Treebind) {}},
		{"an https repository with a key and host keys it does not use", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Auth = newSSHAuth()
		}},
		{"an ssh repository", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.URL = sshRepositoryURL
			tb.Spec.Repository.Auth = newSSHAuth()
		}},
		{"an ssh repository in capitals, which the CRD admits", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.URL = upperSSHRepositoryURL
			tb.Spec.Repository.Auth = newSSHAuth()
		}},
		{"no branch", func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "" }},
		{"a sub path and a custom mount path", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.SubPath = subPathNested
			tb.Spec.Target.MountPath = testCustomPath
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			tt.build(treebind)

			if _, _, err := agentConfigOf(t, treebind); err != nil {
				t.Errorf("the agent refuses the environment of its own pod: %v", err)
			}
		})
	}
}

func TestTheAgentRefusesAnSSHRepositoryThatTheOperatorGaveNoFilesFor(t *testing.T) {
	// The CRD does not let this Treebind in, so the pod never has this environment. This is
	// the agent's own check, for a pod that was set up by hand.
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Repository.URL = sshRepositoryURL

	_, _, err := agentConfigOf(t, treebind)

	if err == nil || !strings.Contains(err.Error(), envSSHKeyFile+" is not set") {
		t.Errorf("error = %v, want the agent to say the key is not set", err)
	}
}

// branchCandidates are values for repository.branch that sit on both sides of every rule
// of the API server (CEL and the length limit) and of the agent (CheckBranch), which have
// to agree on each of them.
func branchCandidates() []string {
	long := strings.Repeat("a", agentconfig.MaxBranchLength)
	return []string{
		"", branchMain, "master", "develop", branchReleaseSlash, branchRelease, "feature/retry-sync", "a/b/c/d", "v1.0.0",
		"UPPER", "a_b", "a+b", "a=b", "a,b", "a#b", "a%b", "a&b", "a!b", "a$b", "a'b", "a(b)", "a;b", "a<b>", "a|b", "a`b", "a\"b",
		"@", "a@b", "a@", "@a", "a@{", "a@{1}", "é/ü", "日本語", ".", "..", "a..b", "a.", ".a", "a/.b", "a/.", "a/..",
		"a.lock", "a.lock/b", "a/b.lock", "a.locked", "-main", "a-", "-", "/", "/a", "a/", "a//b", "//a",
		"a b", " a", "a ", "a\tb", "a\nb", "a\rb", "a\x00b", "a\x01b", "a\x7fb", "a b", "a b",
		"a~", "a^", "a:b", "a?", "a*", "a[b", "a]b", `a\b`, "a{b}",
		long, long + "a", strings.Repeat("é", agentconfig.MaxBranchLength), strings.Repeat("é", agentconfig.MaxBranchLength+1),
	}
}

// repositoryURLCandidates are addresses that sit on both sides of every rule of the API
// server and of the agent's CheckRepositoryURL.
func repositoryURLCandidates() []string {
	return []string{
		httpsRepositoryURL, "HTTPS://github.com/example/dashboards.git", "https://example.com:8443/a/b",
		"https://example.com", "https://example.com/", "https://[::1]:8443/a.git", "https://example.com/a%20b.git",
		"https://example.com/a?b=c", "https://example.com/a#b", "https://user@example.com/a.git", "https://user:pw@example.com/a.git",
		"ssh://git@github.com/example/dashboards.git", upperSSHRepositoryURL, "ssh://git@example.com:2222/a.git",
		"ssh://example.com/a.git", "ssh:///a.git", "ssh://git:secret@example.com/a.git", "ssh://git@[::1]/a.git", "ssh://git@example.com/~user/a.git",
		httpRepositoryURL, "HTTP://git.internal/a.git", "http://user@git.internal/a.git",
		"git://example.com/a.git", "file:///srv/a.git", "ftp://example.com/a.git", "ext::sh -c id", "javascript:alert(1)",
		"git@github.com:example/dashboards.git", "github.com/example/dashboards.git", "/srv/a.git", "./a.git", "a.git", "ssh", "ssh:", "https:", "https://",
		"", " ", "https://example.com/a b.git", "https://example.com/a\tb", "https://example.com/a\nb", "https://example.com/a\x00b", "https://example.com/a\x7fb",
		" https://example.com/a.git", "https://example.com/a.git ", "https://example.com/é", "https://éxample.com/a.git",
		"https://example.com/" + strings.Repeat("a", 2000), "https://example.com/" + strings.Repeat("a", 2040),
		"https://example.com/a%zz.git", "https://example.com/a%", "https://example.com:abc/a.git", "https://example.com:99999/a.git",
		"ssh://git@example.com:badport/a.git", "ssh://git@example.com:/a.git", "https://[::1/a.git", "https://[zz]/a.git",
		"https://exa mple.com/a.git", "https://example.com/a.git\\x", "ssh://git@example.com/a\\b.git", "https://%/a.git",
		"https://example.com/%00", "ssh://:@example.com/a.git", "ssh://@example.com/a.git", "https://@example.com/a.git",
	}
}

var _ = Describe("repository.branch", func() {
	var counter int

	created := func(ctx context.Context, branch string) (bool, error) {
		GinkgoHelper()
		counter++
		treebind := newTreebind(fmt.Sprintf("branch-%d", counter))
		treebind.Spec.Repository.Branch = branch
		err := k8sClient.Create(ctx, treebind)
		if err == nil {
			Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
		}
		return err == nil, err
	}

	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for _, branch := range branchCandidates() {
			ok, err := created(ctx, branch)
			agentOK := agentconfig.CheckBranch(branch) == nil
			if ok != agentOK {
				Fail(fmt.Sprintf("branch %q: the API server accepts it = %v (%v), the agent accepts it = %v: "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					branch, ok, err, agentOK))
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

var _ = Describe("repository.url", func() {
	var counter int

	// created returns whether the API server accepted a Treebind with the address. An ssh
	// address comes with the credentials it needs, which an address of another scheme may
	// have too.
	created := func(ctx context.Context, address string) (bool, error) {
		GinkgoHelper()
		counter++
		treebind := newTreebind(fmt.Sprintf("url-%d", counter))
		treebind.Spec.Repository.URL = address
		treebind.Spec.Repository.Auth = newSSHAuth()
		err := k8sClient.Create(ctx, treebind)
		if err == nil {
			Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
		}
		return err == nil, err
	}

	// The agent takes every address that the API server takes, but one scheme.
	It("is taken by the agent whenever the API server takes it, but for http://", func() {
		ctx := context.Background()
		var accepted, refused int
		for _, address := range repositoryURLCandidates() {
			ok, _ := created(ctx, address)
			_, agentErr := agentconfig.CheckRepositoryURL(address)
			if ok && agentErr != nil && !strings.EqualFold(strings.SplitN(address, ":", 2)[0], "http") {
				Fail(fmt.Sprintf("address %q: the API server accepts it, the agent refuses it (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					address, agentErr))
			}
			if ok {
				accepted++
			} else {
				refused++
			}
		}
		Expect(accepted).To(BeNumerically(">", 10))
		Expect(refused).To(BeNumerically(">", 10))
	})

	// This is the one place where they differ, and it is a known one: the Runner refuses Git
	// over plain http (NFR-15) and the CRD still admits it. When the CRD stops, this says so.
	It("admits http://, which the agent refuses at start with its reason", func() {
		ctx := context.Background()
		for _, address := range []string{httpRepositoryURL, "HTTP://git.internal/a.git"} {
			ok, err := created(ctx, address)
			_, agentErr := agentconfig.CheckRepositoryURL(address)

			Expect(ok).To(BeTrue(), "%s: %v", address, err)
			Expect(agentErr).To(MatchError(ContainSubstring("NFR-15")), address)
		}
	})
})
