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

package config

import (
	"maps"
	"strings"
	"testing"
)

const (
	nestedSubPath = "team-a/dashboards"
	badSegment    = "must not contain a '..'"

	subject  = "system:serviceaccount:treebind-system:treebind-controller-manager"
	issuer   = "https://kubernetes.default.svc.cluster.local"
	audience = "treebind.example.org/apps/dashboards"
)

func complete() map[string]string {
	return map[string]string{
		EnvNamespace:      "apps",
		EnvName:           "dashboards",
		EnvStatusIssuer:   issuer,
		EnvStatusAudience: audience,
		EnvStatusSubject:  subject,
	}
}

func fromMap(env map[string]string) (Config, error) {
	return FromEnv(func(name string) string { return env[name] })
}

func TestFromEnvReadsTheSettingsAndFillsTheDefaults(t *testing.T) {
	config, err := fromMap(complete())

	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Namespace: "apps", Name: "dashboards", MountPath: DefaultMountPath,
		StatusAddr:     DefaultStatusAddr,
		StatusIssuer:   issuer,
		StatusAudience: audience,
		StatusSubject:  subject,
		StatusKeysFile: DefaultStatusKeys,
	}
	if config != want {
		t.Errorf("got %+v\nwant %+v", config, want)
	}
}

func TestFromEnvTakesWhatTheOperatorSets(t *testing.T) {
	env := complete()
	env[EnvMountPath] = "/data/dashboards"
	env[EnvStatusAddr] = "127.0.0.1:9000"
	env[EnvStatusKeysFile] = "/keys/jwks.json"

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.MountPath != "/data/dashboards" || config.StatusAddr != "127.0.0.1:9000" || config.StatusKeysFile != "/keys/jwks.json" {
		t.Errorf("the settings were not taken: %+v", config)
	}
}

func TestFromEnvRefusesWhatCannotWork(t *testing.T) {
	tests := []struct {
		name   string
		change map[string]string
		want   string
	}{
		{"no namespace", map[string]string{EnvNamespace: ""}, EnvNamespace + " is not set"},
		{"no name", map[string]string{EnvName: ""}, EnvName + " is not set"},
		{"no issuer", map[string]string{EnvStatusIssuer: ""}, EnvStatusIssuer + " is not set"},
		{"no audience", map[string]string{EnvStatusAudience: ""}, EnvStatusAudience + " is not set"},
		{"no subject", map[string]string{EnvStatusSubject: ""}, EnvStatusSubject + " is not set"},
		{"a relative mount path", map[string]string{EnvMountPath: "work"}, EnvMountPath + " must be an absolute path"},
		{"a mount path that climbs out", map[string]string{EnvMountPath: "/work/../etc"}, EnvMountPath + " must be an absolute path"},
		{"a mount path with a double slash", map[string]string{EnvMountPath: "//work"}, EnvMountPath + " must be an absolute path"},
		{"a mount path with a slash at the end", map[string]string{EnvMountPath: "/work/"}, EnvMountPath + " must be an absolute path"},
		{"a relative keys file", map[string]string{EnvStatusKeysFile: "jwks.json"}, EnvStatusKeysFile + " must be an absolute path"},
		{"a keys file that climbs out", map[string]string{EnvStatusKeysFile: "/etc/../jwks.json"}, EnvStatusKeysFile + " must be an absolute path"},
		{"an address with no port", map[string]string{EnvStatusAddr: "localhost"}, EnvStatusAddr + " must be an address with a port"},
		{"an address that is a port only", map[string]string{EnvStatusAddr: "8080"}, EnvStatusAddr + " must be an address with a port"},
		{"an address with an empty port", map[string]string{EnvStatusAddr: "localhost:"}, EnvStatusAddr + " must be an address with a port"},
		{"a port that is a name", map[string]string{EnvStatusAddr: ":http-alt"}, EnvStatusAddr + " must be an address with a port"},
		{"a port that is too large", map[string]string{EnvStatusAddr: ":99999"}, EnvStatusAddr + " must be an address with a port"},
		{"a negative port", map[string]string{EnvStatusAddr: ":-1"}, EnvStatusAddr + " must be an address with a port"},
		{"a subject that is not a service account", map[string]string{EnvStatusSubject: "admin"}, EnvStatusSubject + " must be a service account"},
		{"a subject with no namespace or name", map[string]string{EnvStatusSubject: "system:serviceaccount:"}, EnvStatusSubject + " must be a service account"},
		{"a subject with no name", map[string]string{EnvStatusSubject: "system:serviceaccount:treebind-system"}, EnvStatusSubject + " must be a service account"},
		{"a subject with an empty name", map[string]string{EnvStatusSubject: "system:serviceaccount:treebind-system:"}, EnvStatusSubject + " must be a service account"},
		{"a subject with too many parts", map[string]string{EnvStatusSubject: subject + ":extra"}, EnvStatusSubject + " must be a service account"},
		{"a user, not a service account", map[string]string{EnvStatusSubject: "system:node:worker-1"}, EnvStatusSubject + " must be a service account"},
		{"four parts that are not a service account", map[string]string{EnvStatusSubject: "system:user:treebind-system:operator"}, EnvStatusSubject + " must be a service account"},
		{"a service account of another kind of subject", map[string]string{EnvStatusSubject: "other:serviceaccount:treebind-system:operator"}, EnvStatusSubject + " must be a service account"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := complete()
			maps.Copy(env, test.change)

			_, err := fromMap(env)

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want a problem that says %q", err, test.want)
			}
		})
	}
}

func TestFromEnvTakesTheSubPathAndLeavesItEmptyWithoutOne(t *testing.T) {
	config, err := fromMap(complete())
	if err != nil || config.SubPath != "" {
		t.Errorf("without TREEBIND_SUB_PATH: %+v, %v, want an empty sub path", config, err)
	}

	env := complete()
	env[EnvSubPath] = nestedSubPath
	config, err = fromMap(env)
	if err != nil || config.SubPath != nestedSubPath {
		t.Errorf("with a sub path: %+v, %v", config, err)
	}
}

// The CRD accepts a sub path with spaces in it, and the operator passes it on as it
// is, so the agent must work in the directory the user named and not in a trimmed one.
func TestFromEnvDoesNotTrimTheSubPath(t *testing.T) {
	env := complete()
	env[EnvSubPath] = " a b "

	config, err := fromMap(env)

	if err != nil || config.SubPath != " a b " {
		t.Errorf("sub path = %q, %v, want it untouched", config.SubPath, err)
	}
}

func TestFromEnvRefusesASubPathThatCannotBeOne(t *testing.T) {
	env := complete()
	env[EnvSubPath] = ".treebind/git"

	_, err := fromMap(env)

	if err == nil || !strings.Contains(err.Error(), EnvSubPath+" must not start with '.treebind'") {
		t.Errorf("got %v, want the refusal to name the variable and the reason", err)
	}
}

func TestCheckSubPath(t *testing.T) {
	long := strings.Repeat("a", MaxSubPathLength)
	tests := []struct {
		name    string
		subPath string
		want    string // "" means accepted, else what the problem says
	}{
		{"none", "", ""},
		{"a directory", "grafana", ""},
		{"nested directories", nestedSubPath, ""},
		{"a dot inside a name", "release-1.2", ""},
		{"a dot-prefixed name that is not reserved", ".config", ""},
		{"a name that starts like the state directory", ".treebindx", ""},
		{"a dash after the state directory's name", ".treebind-old", ""},
		{"the state directory's name below the first segment", "a/.treebind", ""},
		{"a name with a space", "my dashboards", ""},
		{"spaces around the path", " a ", ""},
		{"a letter of more than one byte", "é/ü", ""},
		{"the longest sub path", long, ""},
		{"the longest sub path of letters of two bytes", strings.Repeat("é", MaxSubPathLength), ""},
		{"one character too many", long + "a", "must be at most 1024 characters"},
		{"an absolute path", "/data", "must be relative"},
		{"a dot", ".", "must not be '.'"},
		{"a backslash", `a\b`, "must not contain a backslash"},
		{"a double slash", "a//b", "must not contain '//'"},
		{"a parent segment", "../etc", badSegment},
		{"a parent segment inside", "a/../b", badSegment},
		{"a parent segment at the end", "a/..", badSegment},
		{"a dot segment", "a/./b", badSegment},
		{"a .git segment", "a/.git/b", badSegment},
		{"a .git directory", ".git", badSegment},
		{"a slash at the end", "a/", "must not end with '/'"},
		{"the state directory", ".treebind", "must not start with '.treebind'"},
		{"below the state directory", ".treebind/git", "must not start with '.treebind'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSubPath(tt.subPath)

			switch {
			case tt.want == "" && err != nil:
				t.Errorf("CheckSubPath(%q) = %v, want it accepted", tt.subPath, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("CheckSubPath(%q) = %v, want a problem that says %q", tt.subPath, err, tt.want)
			}
		})
	}
}

func TestFromEnvReportsEveryProblemAtOnce(t *testing.T) {
	_, err := fromMap(map[string]string{EnvMountPath: "work", EnvStatusAddr: "nope"})

	if err == nil {
		t.Fatal("an empty environment was accepted")
	}
	for _, want := range []string{
		EnvNamespace + " is not set", EnvName + " is not set", EnvStatusIssuer + " is not set",
		EnvStatusAudience + " is not set", EnvStatusSubject + " is not set",
		EnvMountPath + " must be an absolute path", EnvStatusAddr + " must be an address with a port",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

func TestFromEnvTrimsWhatAFileOrAManifestLeavesAround(t *testing.T) {
	env := complete()
	env[EnvStatusAudience] = " treebind.example.org/apps/dashboards\n"
	env[EnvStatusSubject] = subject + "\r\n"
	env[EnvStatusIssuer] = "\thttps://kubernetes.default.svc.cluster.local "

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.StatusAudience != audience || config.StatusSubject != subject ||
		config.StatusIssuer != issuer {
		t.Errorf("the values were not trimmed: %+v", config)
	}

	env[EnvName] = " \n"
	if _, err := fromMap(env); err == nil || !strings.Contains(err.Error(), EnvName+" is not set") {
		t.Errorf("a value of only whitespace counts as set: %v", err)
	}
}

func TestPortsFrom0To65535AreValid(t *testing.T) {
	for _, address := range []string{":0", ":1", ":8080", "127.0.0.1:65535", "[::1]:9000", "localhost:80"} {
		env := complete()
		env[EnvStatusAddr] = address
		if _, err := fromMap(env); err != nil {
			t.Errorf("%s was refused: %v", address, err)
		}
	}
}
