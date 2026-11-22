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
	"fmt"
	"testing"

	"sigs.k8s.io/yaml"
)

// The metrics of a Treebind carry the label namespace, the namespace of the Treebind. Prometheus gives every target a
// label of that name too, the namespace of the operator, and keeps its own unless the scrape honors the labels of the
// target: the namespace of the Treebind would then be exported_namespace, and the expressions of
// docs/metrics-and-events.md would find nothing. Both ServiceMonitors that the repository ships honor them.

// serviceMonitor is what these tests read of a ServiceMonitor.
type serviceMonitor struct {
	Kind string `json:"kind"`
	Spec struct {
		Endpoints []struct {
			HonorLabels bool `json:"honorLabels"`
		} `json:"endpoints"`
	} `json:"spec"`
}

func expectHonorsLabels(t *testing.T, where string, monitor serviceMonitor) {
	t.Helper()
	if monitor.Kind != "ServiceMonitor" || len(monitor.Spec.Endpoints) == 0 {
		t.Fatalf("%s: %+v is not a ServiceMonitor with endpoints", where, monitor)
	}
	for i, endpoint := range monitor.Spec.Endpoints {
		if !endpoint.HonorLabels {
			t.Errorf("%s: endpoint %d does not honor the labels of the target, so namespace would be the operator's", where, i)
		}
	}
}

func TestTheServiceMonitorOfKustomizeKeepsTheNamespaceOfTheTreebind(t *testing.T) {
	var monitor serviceMonitor
	if err := yaml.Unmarshal([]byte(readRepoFile(t, dirConfig, "prometheus", "monitor.yaml")), &monitor); err != nil {
		t.Fatal(err)
	}
	expectHonorsLabels(t, "config/prometheus/monitor.yaml", monitor)
}

func TestRealHelmMakesAServiceMonitorThatKeepsTheNamespaceOfTheTreebind(t *testing.T) {
	helmBinary(t)
	for _, secure := range []bool{true, false} {
		t.Run(fmt.Sprintf("metrics.secure %v", secure), func(t *testing.T) {
			stdout, stderr, err := helmTemplate(t, map[string]any{
				"prometheus": map[string]any{"enabled": true},
				"metrics":    map[string]any{"secure": secure},
			})
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, stderr)
			}
			found := 0
			for _, document := range helmDocuments(stdout) {
				var monitor serviceMonitor
				if err := yaml.Unmarshal([]byte(document), &monitor); err != nil {
					t.Fatalf("a document of helm template does not parse: %v", err)
				}
				if monitor.Kind == "ServiceMonitor" {
					found++
					expectHonorsLabels(t, "the chart", monitor)
				}
			}
			if found != 1 {
				t.Errorf("the chart made %d ServiceMonitors, want 1", found)
			}
		})
	}
}
