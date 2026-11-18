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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The operator may do what its code does and no more (NFR-5, R-16). Every call that it makes to the API
// server is listed here, and a test holds each role to exactly that list, so that a verb that the code
// does not use cannot come back with a scaffold or a marker. The calls are:
//
//   - the cached reads that For(Treebind) and Owns(StatefulSet, Service) set up: get, list, watch;
//   - Status().Update of a Treebind, in the reconciler and in the poller;
//   - CreateOrUpdate of a StatefulSet and of a Service: get, create, update;
//   - Create and a JSON Patch of a ConfigMap (docs/adr/0015), and no read of one;
//   - the owner reference that the children carry, which sets blockOwnerDeletion and so asks for update
//     on the finalizers of the owner.
//
// It deletes nothing: the garbage collector removes what a deleted Treebind owns.
const (
	treebindsGroup = "treebind.example.org"
	verbUpdate     = "update"

	// dirRBAC and dirTreebind are the names of the directories that the roles are read from.
	dirRBAC          = "rbac"
	dirTreebind      = "treebind"
	servicesResource = "services"
	notUsed          = "which the operator does not use"
)

// managerGrants is what the manager role of the operator grants, as "group/resource" and its verbs.
var managerGrants = map[string][]string{
	treebindsGroup + "/treebinds":            {verbGet, verbList, verbWatch},
	treebindsGroup + "/treebinds/status":     {verbUpdate},
	treebindsGroup + "/treebinds/finalizers": {verbUpdate},
	appsGroup + "/statefulsets":              {verbCreate, verbGet, verbList, verbUpdate, verbWatch},
	"/services":                              {verbCreate, verbGet, verbList, verbUpdate, verbWatch},
	"/" + configMaps:                         {verbCreate, verbPatch},
}

// electionGrants is what leader election needs: a Lease that it reads, creates and renews, and the events
// that it records. It does not use ConfigMaps, which the scaffold lists for locks that the library no
// longer takes.
var electionGrants = map[string][]string{
	leasesGroup + "/" + leasesResource: {verbCreate, verbGet, verbUpdate},
	"/events":                          {verbCreate, verbPatch},
}

// grantsOf flattens rules into "group/resource" keys with the sorted verbs of every rule that names them.
// A rule for several groups or resources counts for each of them, which is how the API server reads it.
func grantsOf(rules []policyRule) map[string][]string {
	grants := map[string][]string{}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := group + "/" + resource
				grants[key] = append(grants[key], rule.Verbs...)
			}
		}
	}
	for key, verbs := range grants {
		slices.Sort(verbs)
		grants[key] = slices.Compact(verbs)
	}
	return grants
}

// grantDifference says, in words, how a set of grants differs from the one that is wanted, and is empty
// when they are the same.
func grantDifference(got, want map[string][]string) string {
	var lines []string
	for _, key := range slices.Sorted(maps.Keys(got)) {
		if wanted, ok := want[key]; !ok {
			lines = append(lines, fmt.Sprintf("%s is granted %v, "+notUsed, key, got[key]))
		} else if !slices.Equal(got[key], wanted) {
			lines = append(lines, fmt.Sprintf("%s is granted %v, want %v", key, got[key], wanted))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if _, ok := got[key]; !ok {
			lines = append(lines, fmt.Sprintf("%s is not granted, and the operator needs %v", key, want[key]))
		}
	}
	return strings.Join(lines, "; ")
}

func TestTheManagerRoleGrantsWhatTheOperatorUsesAndNothingElse(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", dirRBAC, "role.yaml"),
		filepath.Join("..", "..", "charts", dirTreebind, "templates", dirRBAC, "manager-role.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			if diff := grantDifference(grantsOf(rulesFrom(t, path)), managerGrants); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestTheLeaderElectionRoleGrantsWhatALeaseLockUsesAndNothingElse(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", dirRBAC, "leader_election_role.yaml"),
		filepath.Join("..", "..", "charts", dirTreebind, "templates", dirRBAC, "leader-election-role.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			if diff := grantDifference(grantsOf(rulesFrom(t, path)), electionGrants); diff != "" {
				t.Error(diff)
			}
		})
	}
}

// The checks above pass when the detector is broken, so this holds it to a verb added, a verb missing, a
// resource added and a resource missing.
func TestGrantDifference(t *testing.T) {
	want := map[string][]string{"/services": {verbGet, verbList}}
	tests := []struct {
		name string
		got  []policyRule
		diff string
	}{
		{"the same", []policyRule{{[]string{""}, []string{servicesResource}, []string{verbList, verbGet}}}, ""},
		{"the same, in two rules", []policyRule{
			{[]string{""}, []string{servicesResource}, []string{verbGet}}, {[]string{""}, []string{servicesResource}, []string{verbList}},
		}, ""},
		{"a verb more", []policyRule{{[]string{""}, []string{servicesResource}, []string{verbGet, verbList, verbDelete}}}, "want"},
		{"a verb less", []policyRule{{[]string{""}, []string{servicesResource}, []string{verbGet}}}, "want"},
		{"a resource more", []policyRule{
			{[]string{""}, []string{servicesResource, secrets}, []string{verbGet, verbList}},
		}, notUsed},
		{"a group more", []policyRule{
			{[]string{"", appsGroup}, []string{servicesResource}, []string{verbGet, verbList}},
		}, notUsed},
		{"a wildcard", []policyRule{{[]string{""}, []string{"*"}, []string{verbGet, verbList}}}, notUsed},
		{"nothing", nil, "is not granted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff := grantDifference(grantsOf(tt.got), want)
			if (tt.diff == "") != (diff == "") || !strings.Contains(diff, tt.diff) {
				t.Errorf("grantDifference = %q, want it to hold %q", diff, tt.diff)
			}
		})
	}
}

// roleFiles lists the files under a directory of RBAC objects that define a Role or a ClusterRole, by
// the "rules:" that only those have. Bindings and service accounts have none.
func roleFiles(t *testing.T, elem ...string) []string {
	t.Helper()
	dir := filepath.Join(append([]string{"..", ".."}, elem...)...)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), "\nrules:\n") {
			files = append(files, path)
		}
	}
	return files
}

// No role that the operator ships gives any access to Secrets, by name or through a wildcard of a verb, a
// resource or a group (NFR-21): the keys that the agent needs are mounted through the pod spec. It walks
// every role of the kustomize base and of the chart and not only the manager's, so that a role that is added
// later is held to it too.
func TestNoRoleOfTheOperatorReachesSecrets(t *testing.T) {
	for _, walk := range []struct {
		dir   []string
		known []string
	}{
		{[]string{"config", dirRBAC}, []string{"role.yaml", "leader_election_role.yaml", "token_role.yaml"}},
		{[]string{"charts", dirTreebind, "templates", dirRBAC}, []string{"manager-role.yaml", "leader-election-role.yaml", "token-role.yaml"}},
	} {
		files := roleFiles(t, walk.dir...)
		// The walk passes when it finds nothing, so it has to find the roles that are known.
		for _, name := range walk.known {
			if !slices.ContainsFunc(files, func(file string) bool { return filepath.Base(file) == name }) {
				t.Errorf("the walk of %v did not find %s in %v", walk.dir, name, files)
			}
		}
		for _, path := range files {
			t.Run(path, func(t *testing.T) {
				for _, rule := range rulesFrom(t, path) {
					if coversCoreResource(rule, secrets) {
						t.Errorf("a rule reaches Secrets: %+v", rule)
					}
				}
			})
		}
	}
}
