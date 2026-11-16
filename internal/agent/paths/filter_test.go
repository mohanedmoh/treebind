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

package paths

import (
	"slices"
	"strings"
	"testing"
)

// The state directory of the agent, as layout.Layout.NeverSynced names it. The package
// does not import the layout, which imports the agent's settings, which are to check
// patterns with this package.
const stateDir = ".treebind"

// Strings that the tests of the filter repeat.
const (
	patternBak   = "*.bak"
	pathUpperBak = "a.BAK"
	envExample   = ".env.example"
)

// filterTest is a path and the reason a filter gives for it.
type filterTest struct {
	path string
	want Reason
}

func newTestFilter(t *testing.T, include, exclude []string, options Options) *Filter {
	t.Helper()
	f, err := NewFilter(include, exclude, stateDir, options)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func runFilterTests(t *testing.T, f *Filter, tests []filterTest) {
	t.Helper()
	for _, tt := range tests {
		if got := f.Reason(tt.path); got != tt.want {
			t.Errorf("Reason(%q) = %s, want %s", tt.path, got, tt.want)
		}
	}
}

// What is never synced and what is a secret holds whatever the spec says: an include of
// everything does not bring it back.
func TestFilterLeavesOutWhatTheSpecCannotBringBack(t *testing.T) {
	for _, include := range [][]string{nil, {"**"}} {
		runFilterTests(t, newTestFilter(t, include, nil, Options{}), []filterTest{
			{"emb/", NestedRepository},
			{"a/emb/", NestedRepository},
			{".git/config", GitPath},
			{"a/.GIT/config", GitPath},
			{".Git", GitPath},
			{".git.", GitPath},
			{".git .", GitPath},
			{"GIT~1/x", GitPath},
			{".git::$INDEX_ALLOCATION", GitPath},
			{`.git\x`, GitPath},
			{stateDir + "/git/HEAD", StateDirectory},
			{".TreeBind/x", StateDirectory},
			{pathTmp, Transient},
			{"dir/x.swp", Transient},
			{nameEnv, Secret},
			{"a/.ENV", Secret},
			{".env.local", Secret},
			{".env/x", Secret},
			{"id_rsa", Secret},
			{"x/ID_ED25519_SK", Secret},
			{"id_dsa", Secret},
			{"id_ecdsa", Secret},
			{"id_ed25519", Secret},
			{"id_ecdsa_sk", Secret},
			{"tls.key", Secret},
			{"a.P12", Secret},
			{"store.jks", Secret},
			{"cert.pfx", Secret},
			{"x.keystore", Secret},
			{".git-credentials", Secret},
			{".netrc", Secret},
			{"_netrc", Secret},
			{".pgpass", Secret},
			{envExample, Synced},
			{".ENV.Sample", Synced},
			{".env.template", Synced},
			{"id_rsa.pub", Synced},
			{".envrc", Synced},
			{"keys.txt", Synced},
			{"a.keys", Synced},
			{".gitignore", Synced},
			{".github/workflows/ci.yml", Synced},
			{"a.git", Synced},
			{".git.x", Synced},
			{"git~2", Synced},
			{"a/" + stateDir + "/x", Synced},
			{stateDir + "x/y", Synced},
			{pathTmpDir, Synced},
			// Where two rules take a path, the first one in the order says why.
			{".git/", NestedRepository},
			{stateDir + "/.git", GitPath},
			{stateDir + "/a.tmp", StateDirectory},
			{".env.swp", Transient},
		})
	}
}

// The state directory is left out in any case, as the volume may fold case, and only when
// it is in the work tree.
func TestFilterStateDirectory(t *testing.T) {
	for _, tt := range []struct {
		stateDir string
		want     Reason
	}{{"", Synced}, {".TreeBind", StateDirectory}} {
		f, err := NewFilter(nil, nil, tt.stateDir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		runFilterTests(t, f, []filterTest{{stateDir + "/x", tt.want}, {"x", Synced}})
	}
}

func TestFilterIncludesAndExcludes(t *testing.T) {
	f := newTestFilter(t, []string{"dashboards/", "*.json"}, []string{patternBak, "secret.json"}, Options{})
	runFilterTests(t, f, []filterTest{
		{"dashboards/a.yaml", Synced},
		{"x/a.json", Synced},
		{"README.md", NotIncluded},
		{"dashboards/a.bak", Excluded},
		{"secret.json", Excluded},
		{pathUpperBak, NotIncluded},
		{"dashboards/.env", Secret},
		{nameEnv, Secret},
		{"README.bak", NotIncluded},
	})
	runFilterTests(t, newTestFilter(t, nil, []string{patternBak}, Options{}), []filterTest{
		{"README.md", Synced},
		{"a.bak", Excluded},
		{pathUpperBak, Synced},
	})
	runFilterTests(t, newTestFilter(t, nil, []string{patternBak}, Options{FoldCase: true}), []filterTest{
		{pathUpperBak, Excluded},
	})
}

func TestNewFilterNamesTheListAndThePattern(t *testing.T) {
	for _, tt := range []struct {
		include, exclude []string
		want             string
	}{
		{[]string{"/etc"}, nil, `paths: include[0] "/etc" ` + wantRelative},
		{nil, []string{patternBak, "a//b"}, `paths: exclude[1] "a//b" must not have an empty`},
	} {
		_, err := NewFilter(tt.include, tt.exclude, stateDir, Options{})
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("NewFilter(%q, %q) = %v, want an error that starts with %s", tt.include, tt.exclude, err, tt.want)
		}
	}
}

// reasons are all the reasons that a filter gives.
var reasons = []Reason{Synced, GitPath, NestedRepository, StateDirectory, Transient, Secret, NotIncluded, Excluded}

// NewFilter compiles what Check takes, and whatever the pattern and the path, an exclude
// wins over an include and the reason is one of those above. The seeds run as ordinary
// tests, and go test -fuzz=FuzzFilter -fuzztime=1m ./internal/agent/paths/ looks for more.
func FuzzFilter(f *testing.F) {
	for _, seed := range []struct{ pattern, path string }{
		{patternTmp, pathTmp}, {"dashboards/", "dashboards/a.json"}, {"**", ".git/x"}, {"*", "emb/"},
		{nameEnv, "a/.env"}, {"x", stateDir + "/x"}, {"[", "a"}, {"a/**/b", "a/x/b"}, {"x", ""}, {"x", "/"},
	} {
		f.Add(seed.pattern, seed.path)
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		both, err := NewFilter([]string{pattern}, []string{pattern}, stateDir, Options{})
		if (err == nil) != (Check(pattern) == nil) {
			t.Fatalf("NewFilter with %q = %v, and Check = %v", pattern, err, Check(pattern))
		}
		if err == nil && both.Reason(path) == Synced {
			t.Fatalf("%q is both included and excluded, and %q is synced", pattern, path)
		}
		open, err := NewFilter(nil, nil, stateDir, Options{FoldCase: true})
		if err != nil {
			t.Fatal(err)
		}
		if reason := open.Reason(path); !slices.Contains(reasons, reason) {
			t.Fatalf("Reason(%q) = %q, which is not a reason", path, reason)
		}
	})
}

// SkipsDir says that nothing below a directory is synced, by its own name and whatever the lists say: the
// watcher does not watch it. It is not an answer for the patterns, which can leave out only some of the
// files of a directory, and it is not asked about a directory that holds files that a pattern takes.
func TestSkipsDirIsTrueOnlyWhenNothingBelowTheDirectoryCanBeSynced(t *testing.T) {
	f := newTestFilter(t, []string{"keep/only.txt"}, []string{"gone/*", "dir/x", "dir/?", "*.log"}, Options{})
	for dir, want := range map[string]bool{
		".git":              true,
		"a/.git":            true,
		"a/.GIT/b":          true,
		".treebind":         true,
		".treebind/git":     true,
		".Treebind":         true,
		".env":              true,
		"a/id_rsa":          true,
		"a/b/.netrc":        true,
		"a":                 false,
		pathAB:              false,
		"treebind":          false,
		"a/.treebind":       false,
		envExample:          false,
		"keep":              false, // an include names a file in it, and 'x' is not that file
		"gone":              false, // an exclude takes every file in it, which is for the scan: it is not by the name
		"dir":               false, // excludes of the files x and ? in it: the name x is not every name
		"a.log":             false,
		"a/.gitignore.d":    false,
		"a/.github":         false,
		"sub/.git.backup":   false,
		"sub/git~1x":        false,
		"sub/.gitmodules.d": false,
	} {
		if got := f.SkipsDir(dir); got != want {
			t.Errorf("SkipsDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestSkipsDirDoesNotTakeAStateDirectoryThatIsNotInTheWorkTree(t *testing.T) {
	f, err := NewFilter(nil, nil, "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.SkipsDir(".treebind") {
		t.Error("SkipsDir(.treebind) = true with the state directory outside the work tree: it is a directory like another")
	}
	if f.SkipsDir("") || newTestFilter(t, nil, nil, Options{}).SkipsDir("") {
		t.Error("SkipsDir of the synced directory itself is true: it is never skipped")
	}
}

func TestSkipsDirAgreesWithReasonForEveryNameOfAFileInTheDirectory(t *testing.T) {
	f := newTestFilter(t, nil, []string{"*.tmp"}, Options{})
	for _, dir := range []string{".git", "a/.git", ".treebind", ".treebind/lock", ".env", "a/id_rsa", "a", "a/b"} {
		for _, name := range []string{"x", "y.txt", "a/b/c", ".hidden"} {
			synced := f.Reason(dir+"/"+name) == Synced
			if f.SkipsDir(dir) && synced {
				t.Errorf("SkipsDir(%q) is true, and %q below it is synced", dir, name)
			}
		}
	}
}
