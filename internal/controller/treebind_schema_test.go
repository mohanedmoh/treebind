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
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// existingSubPath is the target.subPath the immutability tests create with and
// then try to change.
const existingSubPath = "grafana"

// sshRepositoryURL is a repository URL that needs auth: the schema requires a
// key and known hosts for every ssh:// repository.
const sshRepositoryURL = "ssh://git@github.com/example/dashboards.git"

// Addresses and branches that more than one test uses.
const (
	httpsRepositoryURL    = "https://github.com/example/dashboards.git"
	httpRepositoryURL     = "http://git.internal/example/dashboards.git"
	upperSSHRepositoryURL = "SSH://git@github.com/example/dashboards.git"
	branchMain            = "main"
	branchRelease         = "release-1.2"
	branchReleaseSlash    = "release/1.2"
)

// newSSHAuth returns the auth block an ssh:// repository needs.
func newSSHAuth() *treebindv1alpha1.AuthSpec {
	return &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef()}
}

// newTreebind returns the smallest Treebind the CRD schema accepts, so that a
// test which cares about one field starts from a valid object. It uses an
// https URL, which needs no auth.
func newTreebind(name string) *treebindv1alpha1.Treebind {
	return &treebindv1alpha1.Treebind{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: treebindv1alpha1.TreebindSpec{
			Repository: treebindv1alpha1.RepositorySpec{
				URL:    httpsRepositoryURL,
				Branch: branchMain,
			},
			Target: treebindv1alpha1.TargetSpec{ClaimName: "grafana-data"},
			Sync:   treebindv1alpha1.SyncSpec{Direction: treebindv1alpha1.SyncDirectionBidirectional},
		},
	}
}

// newAgentSpec returns an agent block with every field set. Tests that need one
// field take it from here, so each value is spelled once and a shared slice or
// map is never mutated by two tests.
func newAgentSpec() *treebindv1alpha1.AgentSpec {
	runAsUser, runAsGroup, fsGroup := int64(1000), int64(1000), int64(2000)
	return &treebindv1alpha1.AgentSpec{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		NodeSelector: map[string]string{"disktype": "ssd"},
		Tolerations: []corev1.Toleration{{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "storage",
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		PriorityClassName: "background",
		ImagePullSecrets:  []corev1.LocalObjectReference{{Name: "registry-credentials"}},
		Labels:            map[string]string{"team": "storage"},
		Annotations:       map[string]string{"owner": "storage-team"},
		RunAsUser:         &runAsUser,
		RunAsGroup:        &runAsGroup,
		FSGroup:           &fsGroup,
	}
}

// These tests run against the CRD in config/crd/bases, which envtest installs,
// so they check the generated schema and not only the Go types.
var _ = Describe("Treebind schema", func() {
	ctx := context.Background()

	It("accepts every field of the sample manifest and returns it unchanged", func() {
		tb := newTreebind("schema-valid")
		tb.Spec.Repository.Path = "dashboards"
		tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
			SSHKeySecretRef:        &treebindv1alpha1.KeySelector{Name: "dashboards-deploy-key", Key: "id_ed25519"},
			KnownHostsConfigMapRef: &treebindv1alpha1.KeySelector{Name: "git-known-hosts", Key: "known_hosts"},
		}
		tb.Spec.Target.SubPath = "dashboards"
		tb.Spec.Target.MountPath = "/work"
		tb.Spec.Sync.Interval = &metav1.Duration{Duration: 30 * time.Second}
		tb.Spec.Sync.SettleTime = &metav1.Duration{Duration: 5 * time.Second}
		tb.Spec.Sync.ConflictPolicy = treebindv1alpha1.ConflictPolicyFollowRemote
		tb.Spec.Commit = &treebindv1alpha1.CommitSpec{
			AuthorName:      "treebind",
			AuthorEmail:     "treebind@example.org",
			MessageTemplate: "treebind: sync {{ .Count }} file(s)",
		}
		tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Exclude: []string{"*.tmp", "*.swp"}}
		tb.Spec.Placement.Mode = treebindv1alpha1.PlacementModeStandalone

		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tb), got)).To(Succeed())
		Expect(got.Spec).To(Equal(tb.Spec))
	})

	It("accepts the reserved placement and agent fields and returns them unchanged", func() {
		tb := newTreebind("schema-reserved")
		tb.Spec.Placement.Mode = treebindv1alpha1.PlacementModeSidecar
		tb.Spec.Agent = newAgentSpec()

		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tb), got)).To(Succeed())
		Expect(got.Spec).To(Equal(tb.Spec))
	})

	It("accepts the sample manifest", func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "treebind_v1alpha1_treebind.yaml"))
		Expect(err).NotTo(HaveOccurred())

		tb := &treebindv1alpha1.Treebind{}
		Expect(yaml.UnmarshalStrict(data, tb)).To(Succeed())
		tb.Namespace = "default"

		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })
	})

	It("defaults the sync interval, settle time and conflict policy", func() {
		tb := newTreebind("schema-defaults")
		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tb), got)).To(Succeed())
		Expect(got.Spec.Sync.Interval).To(HaveValue(Equal(metav1.Duration{Duration: 30 * time.Second})))
		Expect(got.Spec.Sync.SettleTime).To(HaveValue(Equal(metav1.Duration{Duration: 5 * time.Second})))
		Expect(got.Spec.Sync.ConflictPolicy).To(Equal(treebindv1alpha1.ConflictPolicyFollowRemote))
	})

	It("defaults the placement to standalone and leaves the agent unset", func() {
		tb := newTreebind("schema-placement-default")
		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tb), got)).To(Succeed())
		Expect(got.Spec.Placement.Mode).To(Equal(treebindv1alpha1.PlacementModeStandalone))
		Expect(got.Spec.Agent).To(BeNil())
	})

	It("defaults the placement mode when the placement is an empty object", func() {
		tb := newTreebind("schema-placement-empty")
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(tb)
		Expect(err).NotTo(HaveOccurred())
		u := &unstructured.Unstructured{Object: raw}
		u.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))
		Expect(unstructured.SetNestedMap(u.Object, map[string]any{}, "spec", "placement")).To(Succeed())

		Expect(k8sClient.Create(ctx, u)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tb), got)).To(Succeed())
		Expect(got.Spec.Placement.Mode).To(Equal(treebindv1alpha1.PlacementModeStandalone))
	})

	It("rejects updates to the immutable target fields", func() {
		tb := newTreebind("schema-immutable")
		tb.Spec.Target.SubPath = existingSubPath
		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		byClaim := tb.DeepCopy()
		byClaim.Spec.Target.ClaimName = "other-data"
		err := k8sClient.Update(ctx, byClaim)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err).To(MatchError(ContainSubstring("claimName is immutable")))

		bySubPath := tb.DeepCopy()
		bySubPath.Spec.Target.SubPath = "elsewhere"
		err = k8sClient.Update(ctx, bySubPath)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err).To(MatchError(ContainSubstring("subPath is immutable")))

		mutable := tb.DeepCopy()
		mutable.Spec.Repository.Branch = "develop"
		Expect(k8sClient.Update(ctx, mutable)).To(Succeed())
	})

	It("rejects setting or clearing the optional subPath after creation", func() {
		withoutSubPath := newTreebind("schema-subpath-unset")
		Expect(k8sClient.Create(ctx, withoutSubPath)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, withoutSubPath)).To(Succeed()) })

		added := withoutSubPath.DeepCopy()
		added.Spec.Target.SubPath = existingSubPath
		err := k8sClient.Update(ctx, added)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err).To(MatchError(ContainSubstring("subPath is immutable")))

		withSubPath := newTreebind("schema-subpath-set")
		withSubPath.Spec.Target.SubPath = existingSubPath
		Expect(k8sClient.Create(ctx, withSubPath)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, withSubPath)).To(Succeed()) })

		cleared := withSubPath.DeepCopy()
		cleared.Spec.Target.SubPath = ""
		err = k8sClient.Update(ctx, cleared)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err).To(MatchError(ContainSubstring("subPath is immutable")))
	})

	It("caps metadata.name so the sync pod name fits in a DNS label", func() {
		tooLong := newTreebind(strings.Repeat("a", 57))
		err := k8sClient.Create(ctx, tooLong)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err).To(MatchError(ContainSubstring("56 characters")))

		longest := newTreebind(strings.Repeat("a", 56))
		Expect(k8sClient.Create(ctx, longest)).To(Succeed())
		Expect(k8sClient.Delete(ctx, longest)).To(Succeed())
	})

	It("stores the status fields through the status subresource", func() {
		tb := newTreebind("schema-status")
		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		syncTime := metav1.NewTime(time.Now().Truncate(time.Second))
		tb.Status = treebindv1alpha1.TreebindStatus{
			ObservedGeneration: tb.Generation,
			LastSyncedCommit:   "9f2c1ab0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
			LastSyncTime:       &syncTime,
		}
		Expect(k8sClient.Status().Update(ctx, tb)).To(Succeed())

		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tb), got)).To(Succeed())
		Expect(got.Status.ObservedGeneration).To(Equal(tb.Generation))
		Expect(got.Status.LastSyncedCommit).To(Equal("9f2c1ab0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9"))
		Expect(got.Status.LastSyncTime).NotTo(BeNil())
		Expect(got.Status.LastSyncTime.Time).To(BeTemporally("==", syncTime.Time))
	})

	DescribeTable("accepts an object that satisfies the schema",
		func(mutate func(*treebindv1alpha1.Treebind)) {
			tb := newTreebind("schema-ok")
			mutate(tb)

			Expect(k8sClient.Create(ctx, tb)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tb)).To(Succeed())
		},
		Entry("with an ssh URL, a key and known hosts", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.URL = sshRepositoryURL
			tb.Spec.Repository.Auth = newSSHAuth()
		}),
		Entry("with an upper-case ssh scheme, a key and known hosts", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.URL = upperSSHRepositoryURL
			tb.Spec.Repository.Auth = newSSHAuth()
		}),
		Entry("with an https URL and an unused key and known hosts", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Auth = newSSHAuth()
		}),
		Entry("with an https URL", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.URL = httpsRepositoryURL
		}),
		Entry("with an http URL", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.URL = httpRepositoryURL
		}),
		Entry("with a nested repository path", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Path = "grafana/dashboards"
		}),
		Entry("with a slash in the branch name", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Branch = "feature/retry-sync"
		}),
		Entry("with a nested subPath", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.SubPath = subPathNested
		}),
		Entry("with a subPath that only looks like the agent's state directory", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.SubPath = subPathLookalike
		}),
		Entry("with a dot-prefixed directory that is not .git in the repository path", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Path = ".github/dashboards"
		}),
		Entry("with dots inside a release branch name", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Branch = branchRelease
		}),
		Entry("with an absolute mountPath", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.MountPath = "/work"
		}),
		Entry("with the 1s minimum interval and settle time", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Sync.Interval = &metav1.Duration{Duration: time.Second}
			tb.Spec.Sync.SettleTime = &metav1.Duration{Duration: time.Second}
		}),
		Entry("with relative glob patterns", func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Paths = &treebindv1alpha1.PathsSpec{
				Include: []string{"dashboards/**/*.json"},
				Exclude: []string{"*.tmp", ".git/**"},
			}
		}),
	)

	DescribeTable("rejects an object that breaks the schema",
		func(mutate func(*treebindv1alpha1.Treebind), field string) {
			tb := newTreebind("schema-invalid")
			mutate(tb)

			err := k8sClient.Create(ctx, tb)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err).To(MatchError(ContainSubstring(field)))
		},
		Entry("without a repository URL",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.URL = "" },
			"spec.repository.url"),
		Entry("without a claim name",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.ClaimName = "" },
			"spec.target.claimName"),
		Entry("with a direction that is neither pull nor bidirectional",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Sync.Direction = "sideways" },
			"spec.sync.direction"),
		Entry("with an unknown conflict policy",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Sync.ConflictPolicy = "merge" },
			"spec.sync.conflictPolicy"),
		Entry("with a key reference that names no key",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
					SSHKeySecretRef: &treebindv1alpha1.KeySelector{Name: "dashboards-deploy-key"},
				}
			},
			"spec.repository.auth.sshKeySecretRef.key"),
		Entry("with a repository URL that has no scheme",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.URL = "github.com/example/dashboards.git" },
			"spec.repository.url"),
		Entry("with an scp-style repository URL",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.URL = "git@github.com:example/dashboards.git" },
			"spec.repository.url"),
		Entry("with a git protocol repository URL",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = "git://github.com/example/dashboards.git"
			},
			"spec.repository.url"),
		Entry("with a file repository URL",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.URL = "file:///srv/git/dashboards.git" },
			"spec.repository.url"),
		Entry("with whitespace in the repository URL",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = "https://github.com/example/dash boards.git"
			},
			"spec.repository.url"),
		Entry("with a password in the repository URL",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = "https://user:secret@github.com/example/dashboards.git"
			},
			"spec.repository.url"),
		Entry("with an absolute repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = "/dashboards" },
			"spec.repository.path"),
		Entry("with a parent segment in the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = "dashboards/../secrets" },
			"spec.repository.path"),
		Entry("with an empty segment in the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = "dashboards//grafana" },
			"spec.repository.path"),
		Entry("with a dot as the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = "." },
			"spec.repository.path"),
		Entry("with a backslash in the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = `dashboards\grafana` },
			"spec.repository.path"),
		Entry("with a parent segment in the subPath",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.SubPath = "../other" },
			"spec.target.subPath"),
		Entry("with a relative mountPath",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.MountPath = "work" },
			"spec.target.mountPath"),
		Entry("with a parent segment in the mountPath",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.MountPath = "/work/../etc" },
			"spec.target.mountPath"),
		Entry("with a double dot in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "release..main" },
			"spec.repository.branch"),
		Entry("with a reflog expression in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "main@{1}" },
			"spec.repository.branch"),
		Entry("with a leading dash in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "-main" },
			"spec.repository.branch"),
		Entry("with a leading slash in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "/main" },
			"spec.repository.branch"),
		Entry("with a .lock suffix in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "main.lock" },
			"spec.repository.branch"),
		Entry("with a trailing slash in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "main/" },
			"spec.repository.branch"),
		Entry("with an interval under a second",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Sync.Interval = &metav1.Duration{Duration: 500 * time.Millisecond}
			},
			"spec.sync.interval"),
		Entry("with a zero settle time",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Sync.SettleTime = &metav1.Duration{} },
			"spec.sync.settleTime"),
		Entry("with an absolute include pattern",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{"/etc/**"}}
			},
			"spec.paths.include[0]"),
		Entry("with a parent segment in an exclude pattern",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Exclude: []string{"foo/../bar"}}
			},
			"spec.paths.exclude[0]"),
		Entry("with an empty include pattern",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{""}}
			},
			"spec.paths.include[0]"),
		Entry("with a token as the only userinfo of an https URL",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = "https://ghp_example@github.com/example/dashboards.git"
			},
			"spec.repository.url"),
		Entry("with a control character in the repository URL",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = "https://github.com/example/dash\x00boards.git"
			},
			"spec.repository.url"),
		Entry("with whitespace in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "main foo" },
			"spec.repository.branch"),
		Entry("with a colon in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "origin:main" },
			"spec.repository.branch"),
		Entry("with a component starting with a dot in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "feature/.hidden" },
			"spec.repository.branch"),
		Entry("with an empty component in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "feature//sync" },
			"spec.repository.branch"),
		Entry("with a trailing dot in the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "release." },
			"spec.repository.branch"),
		Entry("with a lone @ as the branch",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Branch = "@" },
			"spec.repository.branch"),
		Entry("with a .git segment in the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = ".git/hooks" },
			"spec.repository.path"),
		Entry("with a dot segment in the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = "dashboards/./grafana" },
			"spec.repository.path"),
		Entry("with a trailing slash in the repository path",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.Path = "dashboards/" },
			"spec.repository.path"),
		Entry("with a .git segment in the subPath",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.SubPath = "grafana/.git" },
			"spec.target.subPath"),
		Entry("with the agent's state directory as the subPath",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.SubPath = subPathState },
			"spec.target.subPath"),
		Entry("with a subPath inside the agent's state directory",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.SubPath = subPathStateGit },
			"spec.target.subPath"),
		Entry("with a placement mode that is neither standalone nor sidecar",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Placement.Mode = "sideCar" },
			"spec.placement.mode"),
		Entry("with a negative runAsUser",
			func(tb *treebindv1alpha1.Treebind) {
				runAsUser := int64(-1)
				tb.Spec.Agent = &treebindv1alpha1.AgentSpec{RunAsUser: &runAsUser}
			},
			"spec.agent.runAsUser"),
		Entry("with more than 32 tolerations",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Agent = &treebindv1alpha1.AgentSpec{
					Tolerations: make([]corev1.Toleration, 33),
				}
			},
			"spec.agent.tolerations"),
		Entry("with an imagePullSecrets entry that has no name",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Agent = &treebindv1alpha1.AgentSpec{
					ImagePullSecrets: []corev1.LocalObjectReference{{}},
				}
			},
			"spec.agent.imagePullSecrets[0]"),
		Entry("with more than 32 labels",
			func(tb *treebindv1alpha1.Treebind) {
				labels := make(map[string]string, 33)
				for i := range 33 {
					labels[fmt.Sprintf("label-%d", i)] = "value"
				}
				tb.Spec.Agent = &treebindv1alpha1.AgentSpec{Labels: labels}
			},
			"spec.agent.labels"),
		Entry("with an ssh URL and no auth",
			func(tb *treebindv1alpha1.Treebind) { tb.Spec.Repository.URL = sshRepositoryURL },
			"spec.repository"),
		Entry("with an ssh URL and only the key",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = sshRepositoryURL
				tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()}
			},
			"spec.repository"),
		Entry("with an ssh URL and only the known hosts",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = sshRepositoryURL
				tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{KnownHostsConfigMapRef: knownHostsRef()}
			},
			"spec.repository"),
		Entry("with an ssh URL and an empty auth block",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = sshRepositoryURL
				tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{}
			},
			"spec.repository"),
		Entry("with an upper-case ssh scheme and no auth",
			func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = upperSSHRepositoryURL
			},
			"spec.repository"),
	)

	It("tells the user what an ssh repository is missing", func() {
		tb := newTreebind("schema-ssh-message")
		tb.Spec.Repository.URL = sshRepositoryURL
		tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()}

		err := k8sClient.Create(ctx, tb)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err).To(MatchError(ContainSubstring(
			"an ssh:// repository needs auth.sshKeySecretRef and auth.knownHostsConfigMapRef")))
	})

	It("applies the same rule when an existing object is edited into an ssh repository", func() {
		tb := newTreebind("schema-ssh-edit")
		Expect(k8sClient.Create(ctx, tb)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, tb)).To(Succeed()) })

		bare := tb.DeepCopy()
		bare.Spec.Repository.URL = sshRepositoryURL
		err := k8sClient.Update(ctx, bare)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)

		complete := tb.DeepCopy()
		complete.Spec.Repository.URL = sshRepositoryURL
		complete.Spec.Repository.Auth = newSSHAuth()
		Expect(k8sClient.Update(ctx, complete)).To(Succeed())
	})
})
