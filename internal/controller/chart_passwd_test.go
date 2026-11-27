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
	"strings"
	"testing"
)

// arbitraryUserIDVariable is the variable of the operator that turns on the passwd file of the sync pods
// (docs/adr/0042), which cmd/operator reads.
const arbitraryUserIDVariable = "AGENT_ARBITRARY_USER_ID"

// The values of the variable, and the key of the values of the agent in the chart.
const (
	envTrue, envFalse = "true", "false"
	agentValuesKey    = "agent"
)

// The kustomize base, and so install.yaml, sets the variable to false, where a user can see it and change it, and the
// chart sets it from agent.arbitraryUserID, which is false by default: an install that says nothing keeps the pod
// templates of the operator before.
func TestTheBaseAndTheChartLeaveThePasswdFileOffAndTheChartTakesItFromItsValue(t *testing.T) {
	config := configDeployment(t)
	got, ok := envValue(container(t, "config/manager/manager.yaml", config.Spec.Template.Spec.Containers).Env, arbitraryUserIDVariable)
	if !ok || got != envFalse {
		t.Errorf("config/manager/manager.yaml sets %s to %q (set: %v), want \"false\"", arbitraryUserIDVariable, got, ok)
	}
	values := defaultValues(t)
	if values.Agent.ArbitraryUserID == nil || *values.Agent.ArbitraryUserID {
		t.Errorf("agent.arbitraryUserID is %v in values.yaml, want false", values.Agent.ArbitraryUserID)
	}
	template := chartDeployment(t, values)
	fromValues, ok := envValue(container(t, "the chart's Deployment template", template.Spec.Template.Spec.Containers).Env,
		arbitraryUserIDVariable)
	if !ok || !strings.Contains(fromValues, "Values_agent_arbitraryUserID") {
		t.Errorf("%s of the chart is %q (set: %v), which is not made from agent.arbitraryUserID", arbitraryUserIDVariable, fromValues, ok)
	}
}

// Real Helm writes true only for a value of true, and false for the rest, also for a release whose values have no
// agent.arbitraryUserID, as helm upgrade --reuse-values of a release before it gives. The operator stops on a value
// that is not a boolean, so the chart never writes one.
func TestRealHelmGivesTheOperatorTrueOnlyForTrue(t *testing.T) {
	helmBinary(t)
	for name, test := range map[string]struct {
		values map[string]any
		want   string
	}{
		"the default":     {nil, envFalse},
		"true":            {arbitraryUserIDValue(true), envTrue},
		"false":           {arbitraryUserIDValue(false), envFalse},
		"taken away":      {arbitraryUserIDValue(nil), envFalse},
		"the string true": {arbitraryUserIDValue(envTrue), envTrue},
		"another string":  {arbitraryUserIDValue("yes"), envFalse},
	} {
		t.Run(name, func(t *testing.T) {
			output, stderr, err := helmTemplate(t, test.values)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, stderr)
			}
			if got := managerEnv(t, output, arbitraryUserIDVariable); got != test.want {
				t.Errorf("%s is %q, want %q", arbitraryUserIDVariable, got, test.want)
			}
		})
	}
}

// arbitraryUserIDValue is the values of the chart with agent.arbitraryUserID set to the value.
func arbitraryUserIDValue(value any) map[string]any {
	return map[string]any{agentValuesKey: map[string]any{"arbitraryUserID": value}}
}
