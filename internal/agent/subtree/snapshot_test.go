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

package subtree

import (
	"strings"
	"testing"
)

const (
	realCommit = "0123456789abcdef0123456789abcdef01234567"
	dashboards = "grafana/dashboards"
)

// What a snapshot says of itself is read back whole, whatever the directory is called: the CRD takes a name with a newline or a
// quote in it, and the agent must not refuse what the API server takes.
func TestWhatASnapshotSaysOfItselfIsReadBack(t *testing.T) {
	for _, dir := range []string{dashboards, "a", "my dashboards", "é/ü", "line\nbreak/x", `quo"te`, "Treebind-Path: x", "tab\tbed", strings.Repeat("d/", 500)} {
		got, ok := parseSnapshot(snapshotMessage(realCommit, dir))
		if !ok || got.source != realCommit || got.path != dir {
			t.Errorf("snapshot of %q = %+v, %v", dir, got, ok)
		}
	}
}

// A message is a snapshot's only when both trailers are in its last paragraph, once, and the source is the name of a commit. A
// commit of the application's changes has the message of a template that the Treebind sets, and what the template says must not
// make it a snapshot by accident.
func TestAMessageThatIsNotASnapshotIsNotOne(t *testing.T) {
	good := snapshotMessage(realCommit, dashboards)
	tests := map[string]string{
		"no trailers":                 "treebind: sync 3 file(s)",
		"empty":                       "",
		"only the source":             "x\n\nTreebind-Source: " + realCommit + "\n",
		"only the path":               "x\n\nTreebind-Path: \"a\"\n",
		"the source is not a name":    "x\n\nTreebind-Source: nothex\nTreebind-Path: \"a\"\n",
		"the source is too short":     "x\n\nTreebind-Source: 0123\nTreebind-Path: \"a\"\n",
		"the source is in capitals":   "x\n\nTreebind-Source: " + strings.ToUpper(realCommit) + "\nTreebind-Path: \"a\"\n",
		"the path is not quoted":      "x\n\nTreebind-Source: " + realCommit + "\nTreebind-Path: a\n",
		"a source twice":              good + "Treebind-Source: " + realCommit + "\n",
		"a path twice":                good + "Treebind-Path: \"b\"\n",
		"trailers in the first block": good + "\nmore text after them\n",
		"trailers in the subject":     "Treebind-Source: " + realCommit + " Treebind-Path: \"a\"",
		"a path that is empty":        "x\n\nTreebind-Source: " + realCommit + "\nTreebind-Path: \"\"\n",
	}
	for name, message := range tests {
		t.Run(name, func(t *testing.T) {
			if got, ok := parseSnapshot(message); ok {
				t.Errorf("%q was read as a snapshot: %+v", message, got)
			}
		})
	}
}
