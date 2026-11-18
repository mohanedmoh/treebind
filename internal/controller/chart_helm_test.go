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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// These tests run the chart through real Helm, and prove the renderer of chart_render_test.go against it:
// the objects that Helm makes for each way of installing have to be the objects that the renderer makes,
// and the values that the renderer refuses have to be refused by Helm with the message that names the
// entry. They run when HELM_BIN names a Helm, which the chart job of CI sets, with the version that
// release.yml pins, and they skip without it, so a run on a machine that has no Helm does not fail.

const (
	helmBinVariable = "HELM_BIN"
	helmCommand     = "template"
	watchFlagPrefix = "--watch-namespaces="
	managerKey      = "manager"
)

// helmBinary is the Helm that the tests run, or a skip of the test when there is none.
func helmBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(helmBinVariable)
	if bin == "" {
		t.Skip(helmBinVariable + " is not set, so this test does not run real Helm; the chart job of CI sets it")
	}
	return bin
}

// helmTemplate runs helm template for the release of these tests with the values, and returns what it wrote
// and what it said when it failed. Helm keeps its cache and its configuration in a directory of the test.
func helmTemplate(t *testing.T, values map[string]any) (stdout, stderr string, err error) {
	t.Helper()
	bin := helmBinary(t)
	dir := t.TempDir()
	if values == nil {
		values = map[string]any{}
	}
	data, err := yaml.Marshal(values)
	if err != nil {
		t.Fatalf("write the values: %v", err)
	}
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "charts", dirTreebind))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, helmCommand, releaseName, chart, "--namespace", releaseNamespace, "-f", valuesFile)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+dir, "HELM_CONFIG_HOME="+dir, "HELM_DATA_HOME="+dir)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	err = cmd.Run()
	return out.String(), errs.String(), err
}

// documentSeparator splits what helm template writes into its documents.
var documentSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

// helmDocuments splits the output of helm template into the documents that hold something.
func helmDocuments(output string) []string {
	var documents []string
	for _, document := range documentSeparator.Split(output, -1) {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}
	return documents
}

// theRBACObjectsOfTheTests are the names of the objects that the renderer of these tests makes.
var theRBACObjectsOfTheTests = []string{managerRoleName, "treebind-manager-rolebinding", volumeRoleName, "treebind-volume-read-rolebinding"}

// sortedObjects puts objects in an order that does not depend on how they were written.
func sortedObjects(objects []renderedObject) []renderedObject {
	slices.SortFunc(objects, func(a, b renderedObject) int {
		return strings.Compare(a.Kind+"/"+a.Metadata.Namespace+"/"+a.Metadata.Name, b.Kind+"/"+b.Metadata.Namespace+"/"+b.Metadata.Name)
	})
	return objects
}

// objectsOfHelm reads the RBAC objects of the operator out of the output of helm template.
func objectsOfHelm(t *testing.T, output string) []renderedObject {
	t.Helper()
	objects := make([]renderedObject, 0, 8)
	for _, document := range helmDocuments(output) {
		var object renderedObject
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("a document of helm template does not parse: %v\n%s", err, document)
		}
		if slices.Contains(theRBACObjectsOfTheTests, object.Metadata.Name) {
			objects = append(objects, object)
		}
	}
	return sortedObjects(objects)
}

// objectsOfTheRenderer reads the same objects from the renderer of the tests, which runs the four templates.
func objectsOfTheRenderer(t *testing.T, values map[string]any) []renderedObject {
	t.Helper()
	objects := make([]renderedObject, 0, 8)
	for _, file := range []string{managerRoleFile, managerBindingFile, volumeRoleFile, volumeBindingFile} {
		objects = append(objects, rendered(t, file, values)...)
	}
	return sortedObjects(objects)
}

func TestRealHelmMakesTheObjectsThatTheRendererOfTheTestsMakes(t *testing.T) {
	helmBinary(t)
	for _, install := range chartInstalls() {
		t.Run(install.name, func(t *testing.T) {
			output, said, err := helmTemplate(t, install.values)
			if err != nil {
				t.Fatalf("helm template failed: %v\n%s", err, said)
			}
			got, want := objectsOfHelm(t, output), objectsOfTheRenderer(t, install.values)
			if len(want) == 0 {
				t.Fatal("the renderer of the tests made nothing, so there is nothing to compare")
			}
			if !reflect.DeepEqual(got, want) {
				gotYAML, _ := yaml.Marshal(got)
				wantYAML, _ := yaml.Marshal(want)
				t.Errorf("Helm made different objects than the renderer of the tests.\nHelm:\n%s\nrenderer:\n%s", gotYAML, wantYAML)
			}
		})
	}
}

func TestRealHelmRefusesTheValuesThatTheChartRefusesAndNamesTheEntry(t *testing.T) {
	helmBinary(t)
	for _, refusal := range chartRefusals() {
		t.Run(refusal.name, func(t *testing.T) {
			output, said, err := helmTemplate(t, refusal.values)
			if err == nil {
				t.Fatalf("helm template took values that the chart refuses, and wrote:\n%s", output)
			}
			if !strings.Contains(said, refusal.says) {
				t.Errorf("helm template said %q, want a message that holds %q", said, refusal.says)
			}
		})
	}
}

// managerArguments are the arguments of the manager container of the Deployment that helm template wrote.
func managerArguments(t *testing.T, output string) []string {
	t.Helper()
	for _, document := range helmDocuments(output) {
		var object struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string   `json:"name"`
							Args []string `json:"args"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(document), &object); err != nil || object.Kind != "Deployment" {
			continue
		}
		for _, container := range object.Spec.Template.Spec.Containers {
			if container.Name == managerKey {
				return container.Args
			}
		}
	}
	t.Fatal("helm template wrote no Deployment with a manager container")
	return nil
}

// watchFlags are the --watch-namespaces arguments, in the order they come.
func watchFlags(args []string) []string {
	var flags []string
	for _, arg := range args {
		if strings.HasPrefix(arg, watchFlagPrefix) {
			flags = append(flags, arg)
		}
	}
	return flags
}

func TestRealHelmPutsTheFlagOfTheChartAfterTheArgumentsOfTheUser(t *testing.T) {
	helmBinary(t)
	// The user's list of arguments replaces the default one, so it keeps the flag that two replicas need.
	userArguments := []any{"--leader-elect", watchFlagPrefix + "elsewhere"}
	withUserArguments := map[string]any{managerKey: map[string]any{"args": userArguments}}

	t.Run("namespaced: the flag of the chart is the last, and wins", func(t *testing.T) {
		values := mergeValues(namespacedValues(nsApps2, nsTools2), withUserArguments)
		output, said, err := helmTemplate(t, values)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, said)
		}
		flags := watchFlags(managerArguments(t, output))
		want := []string{watchFlagPrefix + "elsewhere", watchFlagPrefix + nsApps2 + "," + nsTools2}
		if !slices.Equal(flags, want) {
			t.Errorf("the manager gets %v, want %v: the flag of the chart after the flag of the user", flags, want)
		}
	})

	t.Run("cluster-wide: the chart adds no flag and the one of the user is used", func(t *testing.T) {
		output, said, err := helmTemplate(t, withUserArguments)
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, said)
		}
		flags := watchFlags(managerArguments(t, output))
		if want := []string{watchFlagPrefix + "elsewhere"}; !slices.Equal(flags, want) {
			t.Errorf("the manager gets %v, want only the flag of the user, %v", flags, want)
		}
	})

	t.Run("namespaced, with no flag of the user: the one flag of the chart", func(t *testing.T) {
		output, said, err := helmTemplate(t, namespacedValues(nsApps2))
		if err != nil {
			t.Fatalf("helm template failed: %v\n%s", err, said)
		}
		if flags, want := watchFlags(managerArguments(t, output)), []string{watchFlagPrefix + nsApps2}; !slices.Equal(flags, want) {
			t.Errorf("the manager gets %v, want %v", flags, want)
		}
	})
}
