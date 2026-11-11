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
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The paths that more than one test names.
const (
	fieldNodeSelector  = "spec.agent.nodeSelector"
	fieldRunAsUser     = "spec.agent.runAsUser"
	fieldPlacementMode = "spec.placement.mode"
)

func TestUnimplementedFields(t *testing.T) {
	zero := int64(0)

	tests := []struct {
		name string
		spec treebindv1alpha1.TreebindSpec
		want []string
	}{
		{
			name: "an empty spec sets nothing",
		},
		{
			name: "standalone placement is implemented",
			spec: treebindv1alpha1.TreebindSpec{
				Placement: treebindv1alpha1.PlacementSpec{Mode: treebindv1alpha1.PlacementModeStandalone},
			},
		},
		{
			name: "sidecar placement is not",
			spec: treebindv1alpha1.TreebindSpec{
				Placement: treebindv1alpha1.PlacementSpec{Mode: treebindv1alpha1.PlacementModeSidecar},
			},
			want: []string{fieldPlacementMode},
		},
		{
			name: "an agent block that sets nothing",
			spec: treebindv1alpha1.TreebindSpec{Agent: &treebindv1alpha1.AgentSpec{}},
		},
		{
			name: "empty collections and empty resources count as unset",
			spec: treebindv1alpha1.TreebindSpec{Agent: &treebindv1alpha1.AgentSpec{
				Resources:        &corev1.ResourceRequirements{},
				NodeSelector:     map[string]string{},
				Tolerations:      []corev1.Toleration{},
				ImagePullSecrets: []corev1.LocalObjectReference{},
				Labels:           map[string]string{},
				Annotations:      map[string]string{},
			}},
		},
		{
			name: "run as user 0 is still a choice",
			spec: treebindv1alpha1.TreebindSpec{Agent: &treebindv1alpha1.AgentSpec{RunAsUser: &zero}},
			want: []string{fieldRunAsUser},
		},
		{
			name: "a field from each block is listed in sorted order",
			spec: treebindv1alpha1.TreebindSpec{
				Placement: treebindv1alpha1.PlacementSpec{Mode: treebindv1alpha1.PlacementModeSidecar},
				Agent:     &treebindv1alpha1.AgentSpec{NodeSelector: newAgentSpec().NodeSelector},
			},
			want: []string{fieldNodeSelector, fieldPlacementMode},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unimplementedFields(&tt.spec)
			if !slices.Equal(got, tt.want) {
				t.Errorf("unimplementedFields() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUnimplementedFieldsAgent checks that each agent field is reported under
// its own path, and that setting all of them lists every path once, sorted.
func TestUnimplementedFieldsAgent(t *testing.T) {
	full := newAgentSpec()
	fields := []struct {
		path string
		set  func(*treebindv1alpha1.AgentSpec)
	}{
		{"spec.agent.resources", func(a *treebindv1alpha1.AgentSpec) { a.Resources = full.Resources }},
		{fieldNodeSelector, func(a *treebindv1alpha1.AgentSpec) { a.NodeSelector = full.NodeSelector }},
		{"spec.agent.tolerations", func(a *treebindv1alpha1.AgentSpec) { a.Tolerations = full.Tolerations }},
		{"spec.agent.priorityClassName", func(a *treebindv1alpha1.AgentSpec) { a.PriorityClassName = full.PriorityClassName }},
		{"spec.agent.imagePullSecrets", func(a *treebindv1alpha1.AgentSpec) { a.ImagePullSecrets = full.ImagePullSecrets }},
		{"spec.agent.labels", func(a *treebindv1alpha1.AgentSpec) { a.Labels = full.Labels }},
		{"spec.agent.annotations", func(a *treebindv1alpha1.AgentSpec) { a.Annotations = full.Annotations }},
		{fieldRunAsUser, func(a *treebindv1alpha1.AgentSpec) { a.RunAsUser = full.RunAsUser }},
		{"spec.agent.runAsGroup", func(a *treebindv1alpha1.AgentSpec) { a.RunAsGroup = full.RunAsGroup }},
		{"spec.agent.fsGroup", func(a *treebindv1alpha1.AgentSpec) { a.FSGroup = full.FSGroup }},
	}

	for _, field := range fields {
		t.Run(field.path, func(t *testing.T) {
			agent := &treebindv1alpha1.AgentSpec{}
			field.set(agent)

			got := unimplementedFields(&treebindv1alpha1.TreebindSpec{Agent: agent})
			if want := []string{field.path}; !slices.Equal(got, want) {
				t.Errorf("unimplementedFields() = %v, want %v", got, want)
			}
		})
	}

	t.Run("every field", func(t *testing.T) {
		agent := &treebindv1alpha1.AgentSpec{}
		want := make([]string, 0, len(fields)+1)
		want = append(want, fieldPlacementMode)
		for _, field := range fields {
			field.set(agent)
			want = append(want, field.path)
		}
		slices.Sort(want)

		got := unimplementedFields(&treebindv1alpha1.TreebindSpec{
			Placement: treebindv1alpha1.PlacementSpec{Mode: treebindv1alpha1.PlacementModeSidecar},
			Agent:     agent,
		})
		if !slices.Equal(got, want) {
			t.Errorf("unimplementedFields() = %v, want %v", got, want)
		}
	})
}

// The list in unimplementedFields is kept by hand. This test fails when a field
// is added to AgentSpec without being added there, which would make the
// condition report the new field as supported.
func TestUnimplementedFieldsCoverEveryAgentField(t *testing.T) {
	agent := newAgentSpec()
	got := unimplementedFields(&treebindv1alpha1.TreebindSpec{Agent: agent})

	value := reflect.ValueOf(agent).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if value.Field(i).IsZero() {
			t.Errorf("newAgentSpec leaves AgentSpec.%s unset; set it so this test covers the field", field.Name)
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if path := "spec.agent." + name; !slices.Contains(got, path) {
			t.Errorf("unimplementedFields does not report %s; add it, or the condition says the field is supported", path)
		}
	}
}

func TestSpecSupportedCondition(t *testing.T) {
	supported := specSupportedCondition(nil, 3)
	if supported.Status != metav1.ConditionTrue || supported.Reason != treebindv1alpha1.ReasonSupported {
		t.Errorf("no fields: got %s/%s, want True/Supported", supported.Status, supported.Reason)
	}
	if supported.ObservedGeneration != 3 {
		t.Errorf("no fields: observedGeneration = %d, want 3", supported.ObservedGeneration)
	}

	ignored := specSupportedCondition([]string{fieldNodeSelector, fieldPlacementMode}, 4)
	if ignored.Status != metav1.ConditionFalse || ignored.Reason != treebindv1alpha1.ReasonNotImplemented {
		t.Errorf("two fields: got %s/%s, want False/NotImplemented", ignored.Status, ignored.Reason)
	}
	wantMessage := "Accepted but ignored until implemented: spec.agent.nodeSelector, spec.placement.mode"
	if ignored.Message != wantMessage {
		t.Errorf("two fields: message = %q, want %q", ignored.Message, wantMessage)
	}
	if ignored.ObservedGeneration != 4 {
		t.Errorf("two fields: observedGeneration = %d, want 4", ignored.ObservedGeneration)
	}
}
