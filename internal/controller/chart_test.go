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
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The chart's manager Deployment is a Helm template, so it cannot be generated
// from config/manager the way the CRD and the role are. This compares the parts
// that have to stay the same: the container and pod settings that the chart takes
// from its default values, and the parts that are fixed text in the template.
// Wiring in the agent image is not checked (#51 and #85), and neither are the
// metrics flags, which the chart builds from its own metrics values.

const (
	managerContainer = "manager"
	defaultContainer = "kubectl.kubernetes.io/default-container"
	controlPlane     = "control-plane"
	healthFlag       = "--health-probe-bind-address=:"
)

// deployment holds the fields of a Deployment that the check reads.
type deployment struct {
	Spec struct {
		Replicas *int32               `json:"replicas"`
		Selector metav1.LabelSelector `json:"selector"`
		Template struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     struct {
				SecurityContext               *corev1.PodSecurityContext `json:"securityContext"`
				TerminationGracePeriodSeconds *int64                     `json:"terminationGracePeriodSeconds"`
				Containers                    []corev1.Container         `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// chartValues holds the default values of the chart that its Deployment uses.
type chartValues struct {
	Manager struct {
		Replicas    int32    `json:"replicas"`
		Args        []string `json:"args"`
		HealthProbe struct {
			Port int32 `json:"port"`
		} `json:"healthProbe"`
		PodSecurityContext            *corev1.PodSecurityContext  `json:"podSecurityContext"`
		SecurityContext               *corev1.SecurityContext     `json:"securityContext"`
		Resources                     corev1.ResourceRequirements `json:"resources"`
		TerminationGracePeriodSeconds *int64                      `json:"terminationGracePeriodSeconds"`
	} `json:"manager"`
}

var (
	// inlineExpression matches a Helm expression inside a line.
	inlineExpression = regexp.MustCompile(`\{\{.*?\}\}`)
	// documentMarker matches the line that separates YAML documents.
	documentMarker = regexp.MustCompile(`(?m)^---[ \t\r]*$`)
)

func readRepoFile(t *testing.T, elem ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, elem...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// configDeployment reads the Deployment from config/manager/manager.yaml, which
// also holds the namespace.
func configDeployment(t *testing.T) deployment {
	t.Helper()
	for _, doc := range documentMarker.Split(readRepoFile(t, "config", "manager", "manager.yaml"), -1) {
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &kind); err != nil {
			t.Fatalf("parse config/manager/manager.yaml: %v", err)
		}
		if kind.Kind == "Deployment" {
			var result deployment
			if err := yaml.Unmarshal([]byte(doc), &result); err != nil {
				t.Fatalf("parse the Deployment in config/manager/manager.yaml: %v", err)
			}
			return result
		}
	}
	t.Fatal("config/manager/manager.yaml has no Deployment")
	return deployment{}
}

func defaultValues(t *testing.T) chartValues {
	t.Helper()
	var values chartValues
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "charts", "treebind", "values.yaml")), &values); err != nil {
		t.Fatalf("parse charts/treebind/values.yaml: %v", err)
	}
	return values
}

// chartDeployment reads the fixed text of the chart's Deployment template. Lines
// that hold only a Helm directive are dropped, which flattens every conditional:
// the content of all its branches stays. That is only sound for text outside a
// conditional, which is all this test compares, so a field that moves under an
// if/else shows up as a key set twice and fails the parse. The few numbers the
// template takes from the values are filled in from the defaults, and every other
// expression becomes a placeholder.
func chartDeployment(t *testing.T, values chartValues) deployment {
	t.Helper()
	var grace int64
	if values.Manager.TerminationGracePeriodSeconds != nil {
		grace = *values.Manager.TerminationGracePeriodSeconds
	}
	numbers := strings.NewReplacer(
		"{{ .Values.manager.healthProbe.port }}", fmt.Sprint(values.Manager.HealthProbe.Port),
		"{{ .Values.manager.replicas }}", fmt.Sprint(values.Manager.Replicas),
		"{{ .Values.manager.terminationGracePeriodSeconds }}", fmt.Sprint(grace),
	)

	var kept []string
	for line := range strings.SplitSeq(readRepoFile(t, "charts", "treebind", "templates", "manager", "manager.yaml"), "\n") {
		if helmDirective.MatchString(line) {
			continue
		}
		kept = append(kept, inlineExpression.ReplaceAllString(numbers.Replace(line), "HELM"))
	}

	rendered := []byte(strings.Join(kept, "\n"))
	// Strict parsing into a generic map reports a key that is set twice, which is
	// what a conditional around a field leaves behind. The lenient parse below
	// would keep the last one and could compare the wrong branch.
	if err := yaml.UnmarshalStrict(rendered, &map[string]any{}); err != nil {
		t.Fatalf("the Deployment template of the chart sets a key twice or does not parse, "+
			"which happens when a field that this test compares sits under a condition: %v", err)
	}

	var result deployment
	if err := yaml.Unmarshal(rendered, &result); err != nil {
		t.Fatalf("parse the Deployment template of the chart: %v", err)
	}
	return result
}

func container(t *testing.T, source string, containers []corev1.Container) corev1.Container {
	t.Helper()
	for _, candidate := range containers {
		if candidate.Name == managerContainer {
			return candidate
		}
	}
	t.Fatalf("%s has no container named %s", source, managerContainer)
	return corev1.Container{}
}

func asJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// sameInChart fails when the chart's value differs from the one in config/manager.
func sameInChart(t *testing.T, field string, config, chart any) {
	t.Helper()
	if asJSON(config) != asJSON(chart) {
		t.Errorf("%s differs\n  config/manager:  %s\n  the chart:       %s", field, asJSON(config), asJSON(chart))
	}
}

func TestChartManagerMatchesConfig(t *testing.T) {
	config := configDeployment(t)
	values := defaultValues(t)
	template := chartDeployment(t, values)

	wanted := container(t, "config/manager/manager.yaml", config.Spec.Template.Spec.Containers)
	fixed := container(t, "the chart's Deployment template", template.Spec.Template.Spec.Containers)

	// Each side must have something to compare, so that a field that moves
	// does not make a check pass on two empty values.
	if wanted.SecurityContext == nil || wanted.LivenessProbe == nil || wanted.ReadinessProbe == nil ||
		len(wanted.Args) == 0 || len(wanted.Ports) == 0 || config.Spec.Template.Spec.SecurityContext == nil {
		t.Fatal("config/manager/manager.yaml no longer sets the security context, the probes, the args or the ports")
	}

	// What the chart takes from its default values.
	chartArgs := append(slices.Clone(values.Manager.Args), healthFlag+fmt.Sprint(values.Manager.HealthProbe.Port))
	for _, arg := range wanted.Args {
		if !slices.Contains(chartArgs, arg) {
			t.Errorf("the chart does not pass the argument %s that config/manager passes; it has %v", arg, chartArgs)
		}
	}
	sameInChart(t, "the container security context", wanted.SecurityContext, values.Manager.SecurityContext)
	sameInChart(t, "the pod security context", config.Spec.Template.Spec.SecurityContext, values.Manager.PodSecurityContext)
	sameInChart(t, "the resources", wanted.Resources, values.Manager.Resources)
	sameInChart(t, "the termination grace period",
		config.Spec.Template.Spec.TerminationGracePeriodSeconds, values.Manager.TerminationGracePeriodSeconds)
	sameInChart(t, "the replica count", config.Spec.Replicas, &values.Manager.Replicas)

	// What is fixed text in the template.
	sameInChart(t, "the liveness probe", wanted.LivenessProbe, fixed.LivenessProbe)
	sameInChart(t, "the readiness probe", wanted.ReadinessProbe, fixed.ReadinessProbe)
	sameInChart(t, "the container ports", wanted.Ports, fixed.Ports)
	sameInChart(t, "the "+defaultContainer+" annotation",
		config.Spec.Template.Metadata.Annotations[defaultContainer], template.Spec.Template.Metadata.Annotations[defaultContainer])
	sameInChart(t, "the "+controlPlane+" label of the pods",
		config.Spec.Template.Metadata.Labels[controlPlane], template.Spec.Template.Metadata.Labels[controlPlane])
	sameInChart(t, "the labels the Deployment selects pods by",
		slices.Sorted(maps.Keys(config.Spec.Selector.MatchLabels)), slices.Sorted(maps.Keys(template.Spec.Selector.MatchLabels)))
	sameInChart(t, "the "+controlPlane+" selector",
		config.Spec.Selector.MatchLabels[controlPlane], template.Spec.Selector.MatchLabels[controlPlane])
}
