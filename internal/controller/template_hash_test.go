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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The agent is given the hash of the pod template it runs in as TREEBIND_TEMPLATE_HASH (#214), and reports it, so that
// the operator can tell which template a report is of. The variable is a function of the rest of the template, so that
// the template changes exactly when it did without it.

// templateHashVariable returns the value of the variable that holds the hash of the template in the pod of a
// StatefulSet, and fails the test unless the agent container has it once.
func templateHashVariable(t *testing.T, statefulSet *appsv1.StatefulSet) string {
	t.Helper()
	value, found := "", 0
	for _, variable := range statefulSet.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == agentconfig.EnvTemplateHash {
			value, found = variable.Value, found+1
		}
	}
	if found != 1 {
		t.Fatalf("the agent container has %s %d times, want once", agentconfig.EnvTemplateHash, found)
	}
	return value
}

// theRestOfTheTemplate returns the pod template of a StatefulSet without the variable that holds its hash.
func theRestOfTheTemplate(statefulSet *appsv1.StatefulSet) corev1.PodTemplateSpec {
	template := statefulSet.Spec.Template.DeepCopy()
	container := &template.Spec.Containers[0]
	rest := make([]corev1.EnvVar, 0, len(container.Env))
	for _, variable := range container.Env {
		if variable.Name != agentconfig.EnvTemplateHash {
			rest = append(rest, variable)
		}
	}
	container.Env = rest
	return *template
}

func TestThePodTemplateHoldsTheHashOfTheRestOfIt(t *testing.T) {
	full := newTreebind(testTreebindName)
	full.Spec.Target.SubPath = subPathNested
	full.Spec.Repository.Path = repoPathDashboards
	full.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
		SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef(),
	}
	full.Spec.Sync.Interval = &metav1.Duration{Duration: time.Minute}
	full.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{pathsJSON}}

	for name, treebind := range map[string]*treebindv1alpha1.Treebind{
		"a Treebind with the defaults":  newTreebind(testTreebindName),
		"a Treebind with every setting": full,
	} {
		t.Run(name, func(t *testing.T) {
			statefulSet := mustDesiredStatefulSet(t, treebind, testImage)

			value := templateHashVariable(t, statefulSet)

			want, err := templateHash(theRestOfTheTemplate(statefulSet))
			if err != nil {
				t.Fatal(err)
			}
			if value != want {
				t.Errorf("%s = %s, want %s, the hash of the pod template without it", agentconfig.EnvTemplateHash, value, want)
			}
			if err := agentconfig.CheckTemplateHash(value); err != nil || value == "" {
				t.Errorf("%s = %q, which the agent refuses or takes for none: %v", agentconfig.EnvTemplateHash, value, err)
			}
			// The annotation is the hash of the template as it is written, the variable in it. A StatefulSet of the
			// operator before has the hash of the template without the variable, so it is never equal to this one, and
			// its template is rewritten once: that is how the sync pod gets the variable.
			whole, err := templateHash(statefulSet.Spec.Template)
			if err != nil {
				t.Fatal(err)
			}
			if got := statefulSet.Annotations[templateHashAnnotation]; got != whole || got == value {
				t.Errorf("annotation = %s, want %s, the hash of the whole template, which is not %s", got, whole, value)
			}
		})
	}
}

// The agent reads the variable the way the kubelet gives it to it, and reports what it read.
func TestTheAgentReadsTheHashItIsGiven(t *testing.T) {
	config, env, err := agentConfigOf(t, newTreebind(testTreebindName))

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	if config.TemplateHash == "" || config.TemplateHash != env[agentconfig.EnvTemplateHash] {
		t.Errorf("the agent reads %q, want the value of %s, %q", config.TemplateHash, agentconfig.EnvTemplateHash,
			env[agentconfig.EnvTemplateHash])
	}
}

// The variable changes when the template does, and when nothing but the template does. A Treebind that says what the
// agent does without it, or changes what it ignores, is the same template, and its pod is not replaced (ADRs 0024,
// 0026, 0031, 0034, 0036). The hash of the whole template, in the annotation, is what decides whether the operator
// writes the template, so each case holds the two to each other.
func TestTheHashVariableChangesWithTheTemplateAndOnlyWithIt(t *testing.T) {
	limit := func(n int32) *int32 { return &n }
	none := func(*treebindv1alpha1.Treebind) {}

	tests := []struct {
		name       string
		image      string
		mutate     func(*treebindv1alpha1.Treebind)
		wantChange bool
	}{
		{name: "the same spec", mutate: none},
		{name: "an explicit /work is the default mount path", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.MountPath = defaultMountPath
		}},
		{name: "a mount path that is not clean is the clean one", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.MountPath = defaultMountPath + "/"
		}},
		{name: "an explicit followRemote is the default policy", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Sync.ConflictPolicy = treebindv1alpha1.ConflictPolicyFollowRemote
		}},
		{name: "an explicit default limit of commits is the default", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Commit = &treebindv1alpha1.CommitSpec{MaxPerHour: limit(int32(agentconfig.DefaultCommitMaxPerHour))}
		}},
		{name: "an empty spec.commit and empty lists say nothing", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Commit = &treebindv1alpha1.CommitSpec{}
			tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{}, Exclude: []string{}}
		}},
		{name: "spec.agent and spec.placement, which the operator ignores", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Agent = newAgentSpec()
			tb.Spec.Placement.Mode = treebindv1alpha1.PlacementModeSidecar
		}},
		{name: "another generation of the same spec", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Generation += 7
		}},
		{name: "another image", image: "example.com/treebind-agent:v2", mutate: none, wantChange: true},
		{name: "another mount path", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.MountPath = testCustomPath
		}, wantChange: true},
		{name: "another claim", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.ClaimName = "other-data"
		}, wantChange: true},
		{name: "a sub path", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Target.SubPath = subPathNested
		}, wantChange: true},
		{name: "a repository path", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Path = repoPathDashboards
		}, wantChange: true},
		{name: "another branch", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Branch = branchRelease
		}, wantChange: true},
		{name: "an interval", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Sync.Interval = &metav1.Duration{Duration: time.Minute}
		}, wantChange: true},
		{name: "followLocal", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Sync.ConflictPolicy = treebindv1alpha1.ConflictPolicyFollowLocal
		}, wantChange: true},
		{name: "a cap on deletions", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Sync.MaxDeletions = limit(5)
		}, wantChange: true},
		{name: "a list of paths", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{pathsJSON}}
		}, wantChange: true},
		{name: "an author of the commits", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Commit = &treebindv1alpha1.CommitSpec{AuthorName: "Sync Bot"}
		}, wantChange: true},
		{name: "another limit of commits", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Commit = &treebindv1alpha1.CommitSpec{MaxPerHour: limit(int32(agentconfig.DefaultCommitMaxPerHour) + 1)}
		}, wantChange: true},
		{name: "an SSH key Secret", mutate: func(tb *treebindv1alpha1.Treebind) {
			tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()}
		}, wantChange: true},
	}

	build := func(image string, mutate func(*treebindv1alpha1.Treebind)) (variable, annotation string) {
		if image == "" {
			image = testImage
		}
		treebind := newTreebind(testTreebindName)
		mutate(treebind)
		statefulSet := mustDesiredStatefulSet(t, treebind, image)
		return templateHashVariable(t, statefulSet), statefulSet.Annotations[templateHashAnnotation]
	}
	baseVariable, baseAnnotation := build("", none)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			variable, annotation := build(tt.image, tt.mutate)

			if changed := variable != baseVariable; changed != tt.wantChange {
				t.Errorf("%s changed = %v, want %v", agentconfig.EnvTemplateHash, changed, tt.wantChange)
			}
			if changed := annotation != baseAnnotation; changed != tt.wantChange {
				t.Errorf("annotation changed = %v, want %v: the pod is replaced exactly when the template changes",
					changed, tt.wantChange)
			}
		})
	}
}
