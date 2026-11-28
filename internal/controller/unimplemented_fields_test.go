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
	fieldFSGroup       = "spec.agent.fsGroup"
	fieldClaims        = "spec.agent.resources.claims"
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
			name: "fsGroup 0 is still a choice",
			spec: treebindv1alpha1.TreebindSpec{Agent: &treebindv1alpha1.AgentSpec{FSGroup: &zero}},
			want: []string{fieldFSGroup},
		},
		{
			name: "a field from each block is listed in sorted order",
			spec: treebindv1alpha1.TreebindSpec{
				Placement: treebindv1alpha1.PlacementSpec{Mode: treebindv1alpha1.PlacementModeSidecar},
				Agent: &treebindv1alpha1.AgentSpec{
					FSGroup:   &zero,
					Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: testResourceClaim}}},
				},
			},
			want: []string{fieldFSGroup, fieldClaims, fieldPlacementMode},
		},
		{
			name: "every field that the operator applies is not listed",
			spec: treebindv1alpha1.TreebindSpec{Agent: func() *treebindv1alpha1.AgentSpec {
				agent := newAgentSpec()
				agent.FSGroup = nil
				return agent
			}()},
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

// The list in unimplementedFields is kept by hand. Each field of AgentSpec either changes the pod template, when it is
// the only one set, or is reported, so that a field added to AgentSpec and not applied is not taken for supported.
func TestEveryAgentFieldIsAppliedOrReported(t *testing.T) {
	full := newAgentSpec()
	without := mustDesiredStatefulSet(t, newTreebind(testTreebindName), testImage).Annotations[templateHashAnnotation]
	value := reflect.ValueOf(full).Elem()
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if value.Field(i).IsZero() {
			t.Errorf("newAgentSpec leaves AgentSpec.%s unset; set it so this test covers the field", field.Name)
			continue
		}
		agent := &treebindv1alpha1.AgentSpec{}
		reflect.ValueOf(agent).Elem().Field(i).Set(value.Field(i))
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Agent = agent

		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		path := "spec.agent." + name
		applied := mustDesiredStatefulSet(t, treebind, testImage).Annotations[templateHashAnnotation] != without
		reported := slices.Contains(unimplementedFields(&treebind.Spec), path)
		if applied == reported {
			t.Errorf("%s: applied to the pod = %v and reported = %v, want exactly one", path, applied, reported)
		}
	}
	claims := &treebindv1alpha1.AgentSpec{Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: testResourceClaim}}}}
	if got := unimplementedFields(&treebindv1alpha1.TreebindSpec{Agent: claims}); !slices.Equal(got, []string{fieldClaims}) {
		t.Errorf("unimplementedFields() of resources.claims = %v, want %v", got, []string{fieldClaims})
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

	ignored := specSupportedCondition([]string{fieldFSGroup, fieldPlacementMode}, 4)
	if ignored.Status != metav1.ConditionFalse || ignored.Reason != treebindv1alpha1.ReasonNotImplemented {
		t.Errorf("two fields: got %s/%s, want False/NotImplemented", ignored.Status, ignored.Reason)
	}
	wantMessage := "Accepted but ignored until implemented: spec.agent.fsGroup, spec.placement.mode"
	if ignored.Message != wantMessage {
		t.Errorf("two fields: message = %q, want %q", ignored.Message, wantMessage)
	}
	if ignored.ObservedGeneration != 4 {
		t.Errorf("two fields: observedGeneration = %d, want 4", ignored.ObservedGeneration)
	}
}
