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
	"time"
)

const (
	nestedSubPath = "team-a/dashboards"
	badSegment    = "must not contain a '..'"

	subject  = "system:serviceaccount:treebind-system:treebind-controller-manager"
	issuer   = "https://kubernetes.default.svc.cluster.local"
	audience = "treebind.example.org/apps/dashboards"
	repoURL  = "https://example.com/acme/dashboards.git"
	sshURL   = "ssh://git@example.com/acme/dashboards.git"
	keyFile  = "/etc/treebind/ssh/id_ed25519"
	hostFile = "/etc/treebind/known-hosts/known_hosts"
	podName  = "dashboards-sync-0"

	branchRelease   = "release/1.2"
	problemScheme   = "must use ssh:// or https://"
	problemControl  = "whitespace or control"
	problemLeast    = "must be at least 1s"
	problemDuration = "must be a duration"
)

func complete() map[string]string {
	return map[string]string{
		EnvNamespace:      "apps",
		EnvName:           "dashboards",
		EnvStatusIssuer:   issuer,
		EnvStatusAudience: audience,
		EnvStatusSubject:  subject,
		EnvRepoURL:        repoURL,
		EnvSyncDirection:  DirectionBidirectional,
		EnvPodName:        podName,
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
		Namespace: "apps", Name: "dashboards", MountPath: DefaultMountPath, RepoURL: repoURL,
		SyncInterval: DefaultSyncInterval, SyncDirection: DirectionBidirectional, PodName: podName,
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

func TestFromEnvTakesTheRepositoryTheBranchAndTheFilesOfTheCredentials(t *testing.T) {
	env := complete()
	env[EnvRepoURL] = sshURL
	env[EnvRepoBranch] = branchRelease
	env[EnvSSHKeyFile] = keyFile
	env[EnvKnownHostsFile] = hostFile

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.RepoURL != sshURL || config.RepoBranch != branchRelease || config.SSHKeyFile != keyFile || config.KnownHostsFile != hostFile {
		t.Errorf("the settings were not taken: %+v", config)
	}
}

func TestFromEnvLeavesTheBranchEmptyForTheDefaultBranchAndTakesFilesItDoesNotNeed(t *testing.T) {
	// An https:// repository has no use for the key and the host keys, and the CRD admits both
	// next to it, so the agent does not refuse them.
	env := complete()
	env[EnvSSHKeyFile] = keyFile
	env[EnvKnownHostsFile] = hostFile

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.RepoBranch != "" || config.RepoURL != repoURL {
		t.Errorf("settings = %+v, want the default branch and the address", config)
	}
}

func TestFromEnvRefusesARepositoryThatCannotBeSyncedWith(t *testing.T) {
	tests := []struct {
		name   string
		change map[string]string
		want   string
	}{
		{"no repository", map[string]string{EnvRepoURL: ""}, EnvRepoURL + " is not set"},
		{"a repository of spaces", map[string]string{EnvRepoURL: " \n"}, EnvRepoURL + " is not set"},
		{"plain http", map[string]string{EnvRepoURL: "http://git.internal/acme/dashboards.git"}, "must not use http://"},
		{"another scheme", map[string]string{EnvRepoURL: "git://example.com/acme/dashboards.git"}, problemScheme},
		{"a local path", map[string]string{EnvRepoURL: "file:///srv/dashboards.git"}, problemScheme},
		{"the scp shorthand", map[string]string{EnvRepoURL: "git@example.com:acme/dashboards.git"}, "explicit scheme"},
		{"no scheme", map[string]string{EnvRepoURL: "example.com/acme/dashboards.git"}, problemScheme},
		{"a space inside", map[string]string{EnvRepoURL: "https://example.com/a b.git"}, problemControl},
		{"a control character", map[string]string{EnvRepoURL: "https://example.com/a\x01b.git"}, problemControl},
		{"an ssh repository without a key", map[string]string{EnvRepoURL: sshURL, EnvKnownHostsFile: hostFile}, EnvSSHKeyFile + " is not set"},
		{"an ssh repository without host keys", map[string]string{EnvRepoURL: sshURL, EnvSSHKeyFile: keyFile}, EnvKnownHostsFile + " is not set"},
		{"an ssh repository with neither", map[string]string{EnvRepoURL: sshURL}, EnvSSHKeyFile + " is not set"},
		{"an ssh repository in capitals without a key", map[string]string{
			EnvRepoURL: "SSH://git@example.com/acme/dashboards.git", EnvKnownHostsFile: hostFile,
		}, EnvSSHKeyFile + " is not set"},
		{"a relative key file", map[string]string{EnvSSHKeyFile: "id_ed25519"}, EnvSSHKeyFile + " must be an absolute path"},
		{"a key file that climbs out", map[string]string{EnvSSHKeyFile: "/etc/treebind/../id"}, EnvSSHKeyFile + " must be an absolute path"},
		{"relative host keys", map[string]string{EnvKnownHostsFile: "known_hosts"}, EnvKnownHostsFile + " must be an absolute path"},
		{"a branch that starts with a dash", map[string]string{EnvRepoBranch: "-x"}, EnvRepoBranch + " must not start with '-'"},
		{"a branch with a colon", map[string]string{EnvRepoBranch: "a:b"}, EnvRepoBranch + " must not contain whitespace"},
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

func TestFromEnvSaysTheMissingFilesOfAnSSHRepositoryWhateverElseIsWrongWithIt(t *testing.T) {
	env := complete()
	env[EnvRepoURL] = "ssh://git@example.com:badport/acme/dashboards.git"

	_, err := fromMap(env)

	if err == nil {
		t.Fatal("an address with a bad port was accepted")
	}
	for _, want := range []string{EnvRepoURL, EnvSSHKeyFile + " is not set", EnvKnownHostsFile + " is not set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q, and a user would have to fix one problem to meet the next:\n%v", want, err)
		}
	}
}

func TestFromEnvDoesNotAskForTheFilesOfAnAddressThatOnlyStartsLikeSSH(t *testing.T) {
	env := complete()
	env[EnvRepoURL] = "ssl://example.com/acme/dashboards.git"

	_, err := fromMap(env)

	if err == nil || !strings.Contains(err.Error(), problemScheme) || strings.Contains(err.Error(), EnvSSHKeyFile) {
		t.Errorf("error = %v, want the scheme and nothing about the key", err)
	}
}

func TestFromEnvSaysAMissingRepositoryOnce(t *testing.T) {
	env := complete()
	env[EnvRepoURL] = ""

	_, err := fromMap(env)

	if err == nil || strings.Count(err.Error(), EnvRepoURL) != 1 {
		t.Errorf("error = %v, want one problem about %s: an address that is not there has no scheme to complain of", err, EnvRepoURL)
	}
}

func TestCheckRepositoryURL(t *testing.T) {
	tests := []struct {
		name, url, scheme, want string
	}{
		{"ssh", sshURL, SchemeSSH, ""},
		{"ssh with a port", "ssh://git@example.com:2222/acme/d.git", SchemeSSH, ""},
		{"ssh in capitals, which the CRD admits", "SSH://git@example.com/acme/d.git", SchemeSSH, ""},
		{"https", repoURL, SchemeHTTPS, ""},
		{"https in capitals", "HTTPS://example.com/acme/d.git", SchemeHTTPS, ""},
		{"https with a port and no .git", "https://example.com:8443/acme/d", SchemeHTTPS, ""},
		{"an ssh address without a host, which Git refuses and the CRD admits", "ssh:///acme/d.git", SchemeSSH, ""},
		{"http", "http://example.com/acme/d.git", "", "NFR-15"},
		{"http in capitals", "HTTP://example.com/acme/d.git", "", "NFR-15"},
		{"git", "git://example.com/acme/d.git", "", "ssh:// or https://"},
		{"ftp", "ftp://example.com/d.git", "", "ssh:// or https://"},
		{"nothing but a scheme", "ssh:", SchemeSSH, ""},
		{"a tab", "https://example.com/\td", "", "whitespace"},
		{"a DEL", "https://example.com/\x7fd", "", problemControl},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme, err := CheckRepositoryURL(tt.url)

			switch {
			case tt.want == "" && (err != nil || scheme != tt.scheme):
				t.Errorf("CheckRepositoryURL(%q) = %q, %v, want %s", tt.url, scheme, err, tt.scheme)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("CheckRepositoryURL(%q) = %q, %v, want a problem that says %q", tt.url, scheme, err, tt.want)
			}
		})
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

// What CheckBranch says, in the words that tell one rule from another.
const (
	problemDots       = "'..' or '@{'"
	problemEnding     = "'.lock', '/' or '.'"
	problemCharacters = "whitespace, control characters"
)

func TestCheckBranch(t *testing.T) {
	long := strings.Repeat("a", MaxBranchLength)
	tests := []struct {
		name   string
		branch string
		want   string // "" means accepted, else what the problem says
	}{
		{"none, which is the default branch", "", ""},
		{"a name", "main", ""},
		{"a name with a slash", branchRelease, ""},
		{"a dot and a dash inside", "feature-1.x", ""},
		{"a letter of more than one byte", "é/ü", ""},
		{"the longest branch", long, ""},
		{"one character too many", long + "a", "must be at most 255 characters"},
		{"two dots", "a..b", problemDots},
		{"a reflog selector", "a@{1}", problemDots},
		{"only an at sign", "@", problemDots},
		{"a leading dash, which Git would take for an option", "-x", "'-' or '/'"},
		{"a leading slash", "/a", "'-' or '/'"},
		{"a .lock ending", "a.lock", problemEnding},
		{"a trailing slash", "a/", problemEnding},
		{"a trailing dot", "a.", problemEnding},
		{"a double slash", "a//b", "'//'"},
		{"a space", "a b", problemCharacters},
		{"a tab", "a\tb", problemCharacters},
		{"a newline", "a\nb", problemCharacters},
		{"a NUL", "a\x00b", problemCharacters},
		{"a DEL", "a\x7fb", problemCharacters},
		{"a tilde", "a~1", problemCharacters},
		{"a caret", "a^", problemCharacters},
		{"a colon, which would end the refspec", "a:b", problemCharacters},
		{"a question mark", "a?", problemCharacters},
		{"an asterisk", "a*", problemCharacters},
		{"a bracket", "a[", problemCharacters},
		{"a backslash", `a\b`, problemCharacters},
		{"a component that starts with a dot", "a/.b", "starts with '.'"},
		{"a first component that starts with a dot", ".a", "starts with '.'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckBranch(tt.branch)

			switch {
			case tt.want == "" && err != nil:
				t.Errorf("CheckBranch(%q) = %v, want it accepted", tt.branch, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("CheckBranch(%q) = %v, want a problem that says %q", tt.branch, err, tt.want)
			}
		})
	}
}

func TestFromEnvTakesHowToSyncAndWhoThePodIs(t *testing.T) {
	env := complete()
	env[EnvSyncInterval] = "2m"
	env[EnvSyncDirection] = DirectionPull
	env[EnvPodName] = "another-sync-0"

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.SyncInterval != 2*time.Minute || config.SyncDirection != DirectionPull || config.PodName != "another-sync-0" {
		t.Errorf("the settings were not taken: %+v", config)
	}
}

func TestFromEnvRefusesWhatCannotBeSyncedWith(t *testing.T) {
	tests := []struct {
		name   string
		change map[string]string
		want   string
	}{
		{"no direction", map[string]string{EnvSyncDirection: ""}, EnvSyncDirection + " is not set"},
		{"a direction that is not one", map[string]string{EnvSyncDirection: "push"}, EnvSyncDirection + " must be pull or bidirectional"},
		{"a direction in capitals", map[string]string{EnvSyncDirection: "Pull"}, EnvSyncDirection + " must be pull or bidirectional"},
		{"no pod name", map[string]string{EnvPodName: ""}, EnvPodName + " is not set"},
		{"an interval that is not a duration", map[string]string{EnvSyncInterval: "soon"}, EnvSyncInterval + " must be a duration such as 30s or 5m"},
		{"an interval in days", map[string]string{EnvSyncInterval: "1d"}, EnvSyncInterval + " " + problemDuration},
		{"an interval of nothing but a number", map[string]string{EnvSyncInterval: "30"}, EnvSyncInterval + " " + problemDuration},
		{"an interval below the least", map[string]string{EnvSyncInterval: "999ms"}, EnvSyncInterval + " " + problemLeast},
		{"an interval of nothing", map[string]string{EnvSyncInterval: "0s"}, EnvSyncInterval + " " + problemLeast},
		{"a negative interval", map[string]string{EnvSyncInterval: "-30s"}, EnvSyncInterval + " " + problemLeast},
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

func TestFromEnvSaysABadIntervalWithTheOtherProblems(t *testing.T) {
	env := complete()
	env[EnvSyncInterval] = "soon"
	env[EnvNamespace] = ""

	_, err := fromMap(env)

	if err == nil || !strings.Contains(err.Error(), EnvSyncInterval) || !strings.Contains(err.Error(), EnvNamespace+" is not set") {
		t.Errorf("error = %v, want both problems at once", err)
	}
}

func TestCheckSyncInterval(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
		err  string
	}{
		{"", DefaultSyncInterval, ""},
		{"30s", 30 * time.Second, ""},
		{"1s", time.Second, ""},
		{"1.5s", 1500 * time.Millisecond, ""},
		{"1m0s", time.Minute, ""},
		{"1h30m", 90 * time.Minute, ""},
		{"2562047h47m16.854775807s", time.Duration(1<<63 - 1), ""},
		{"1000ms", time.Second, ""},
		{"999ms", 0, problemLeast},
		{"0s", 0, problemLeast},
		{"-1s", 0, problemLeast},
		{"0", 0, problemLeast},
		{"30", 0, problemDuration},
		{"1d", 0, problemDuration},
		{" 30s", 0, problemDuration},
		{"30 s", 0, problemDuration},
		{"s", 0, problemDuration},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := CheckSyncInterval(tt.raw)

			switch {
			case tt.err == "" && (err != nil || got != tt.want):
				t.Errorf("CheckSyncInterval(%q) = %v, %v, want %v", tt.raw, got, err, tt.want)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("CheckSyncInterval(%q) = %v, %v, want a problem that says %q", tt.raw, got, err, tt.err)
			}
		})
	}
}

func TestCheckSyncDirection(t *testing.T) {
	for _, accepted := range []string{DirectionPull, DirectionBidirectional, "pull", "bidirectional"} {
		if err := CheckSyncDirection(accepted); err != nil {
			t.Errorf("CheckSyncDirection(%q) = %v, want it accepted", accepted, err)
		}
	}
	for _, refused := range []string{"", "push", "Pull", " pull", "pull ", "both", "bidirectional "} {
		if err := CheckSyncDirection(refused); err == nil {
			t.Errorf("CheckSyncDirection(%q) was accepted", refused)
		}
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
