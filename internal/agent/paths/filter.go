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
	"fmt"
	"slices"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/transient"
)

// Reason says whether a file is synced and, when it is not, which rule leaves it out
// (docs/adr/0022). The reasons are for the agent's log and its events; nothing reports
// them yet.
type Reason string

const (
	// Synced is a file that the agent syncs.
	Synced Reason = "Synced"
	// GitPath is a path with a segment that Git refuses to stage (gitName).
	GitPath Reason = "GitPath"
	// NestedRepository is a directory that is a repository of its own.
	NestedRepository Reason = "NestedRepository"
	// StateDirectory is a file in the agent's own state directory.
	StateDirectory Reason = "StateDirectory"
	// Transient is a temporary, swap or lock file of an editor (#35).
	Transient Reason = "Transient"
	// Secret is a file that holds a secret by what it is called (secretName).
	Secret Reason = "Secret"
	// NotIncluded is a file that no include pattern takes, when there are some.
	NotIncluded Reason = "NotIncluded"
	// Excluded is a file that an exclude pattern takes.
	Excluded Reason = "Excluded"
)

// Filter says which files of the synced directory are synced: the rules that hold
// whatever the spec says, then the include and exclude patterns of spec.paths. It does
// not change once it is made, so it is safe for concurrent use.
type Filter struct {
	include, exclude []*Pattern
	// stateDir is the name of the state directory in lower case, or "".
	stateDir string
}

// NewFilter returns the filter of the include and exclude patterns of a Treebind.
// stateDir is the name of the agent's state directory when it is at the top of the
// synced directory, which is what layout.Layout.NeverSynced says, and "" when it is not.
func NewFilter(include, exclude []string, stateDir string, options Options) (*Filter, error) {
	f := &Filter{stateDir: foldASCII(stateDir)}
	var err error
	if f.include, err = compileAll("include", include, options); err != nil {
		return nil, err
	}
	if f.exclude, err = compileAll("exclude", exclude, options); err != nil {
		return nil, err
	}
	return f, nil
}

func compileAll(list string, patterns []string, options Options) ([]*Pattern, error) {
	compiled := make([]*Pattern, 0, len(patterns))
	for i, pattern := range patterns {
		p, err := Compile(pattern, options)
		if err != nil {
			return nil, fmt.Errorf("paths: %s[%d] %q %w", list, i, pattern, err)
		}
		compiled = append(compiled, p)
	}
	return compiled, nil
}

// Reason returns Synced for a file that is synced, and otherwise the first rule that
// leaves it out: the ones that hold whatever the spec says, then the transient and
// secret files, then the include patterns when there are some, then the exclude
// patterns. So an exclude wins over an include, and neither can bring back a file that
// the rules before them leave out.
//
// The path is one that git status --porcelain=v2 -z --untracked-files=all lists, for a
// change of any kind and whether the file is tracked or not: relative to the synced
// directory, with slashes, and with a slash at the end for a directory. Status lists a
// directory only when it is a repository of its own, which git add would stage as a
// gitlink, a submodule by another name.
func (f *Filter) Reason(path string) Reason {
	if strings.HasSuffix(path, "/") {
		return NestedRepository
	}
	// When the whole claim is synced, the state directory is in the work tree, and status
	// lists the files of the Git directory in it. When it is not, stateDir is "", which no
	// name in a path is.
	segments := strings.Split(path, "/")
	switch {
	case slices.ContainsFunc(segments, gitName):
		return GitPath
	case foldASCII(segments[0]) == f.stateDir:
		return StateDirectory
	case transient.IsTransient(path):
		return Transient
	case slices.ContainsFunc(segments, secretName):
		return Secret
	case len(f.include) > 0 && !matchesAny(f.include, path):
		return NotIncluded
	case matchesAny(f.exclude, path):
		return Excluded
	}
	return Synced
}

// SkipsDir reports whether no file below a directory is ever synced, by the names in the path of the
// directory alone: a .git segment, the state directory, and a name that is a secret. The watcher does not
// watch such a directory. It says nothing of the patterns, which can take some of the files of a directory
// and not others, and the files that a directory has are for the scan, which is Reason's. The directory is
// relative to the synced directory, with slashes and none at the end.
func (f *Filter) SkipsDir(dir string) bool {
	segments := strings.Split(dir, "/")
	return slices.ContainsFunc(segments, gitName) ||
		f.stateDir != "" && foldASCII(segments[0]) == f.stateDir ||
		slices.ContainsFunc(segments, secretName)
}

func matchesAny(patterns []*Pattern, path string) bool {
	return slices.ContainsFunc(patterns, func(p *Pattern) bool { return p.Match(path) })
}

// gitName reports whether a segment of a path is a name that Git refuses to stage: .git
// in any case, and what a Windows file system reads as .git, which Git refuses on Linux
// too (core.protectNTFS): .git or its short name git~1, with dots or spaces after it,
// and with anything after a ':' or a '\'. One file with such a name in the work tree
// makes git add -A fail and stage nothing.
func gitName(segment string) bool {
	name := foldASCII(segment)
	rest, ok := strings.CutPrefix(name, ".git")
	if !ok {
		if rest, ok = strings.CutPrefix(name, "git~1"); !ok {
			return false
		}
	}
	if i := strings.IndexAny(rest, `:\`); i >= 0 {
		rest = rest[:i]
	}
	return strings.Trim(rest, ". ") == ""
}

// The names of files that hold a secret by what they are called, which the agent never
// syncs (R-1): the credential stores of Git, of netrc and of PostgreSQL, environment
// files, private ssh keys, private keys and key stores. They are left out at any depth
// and in any case, for a directory as for a file, as a pattern without a slash would
// be. A secret in a file with another name is for the scan before a push (#65).
var (
	secretNames = []string{
		".git-credentials", ".netrc", "_netrc", ".pgpass", ".env",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "id_ecdsa_sk", "id_ed25519_sk",
	}
	secretSuffixes = []string{".key", ".p12", ".pfx", ".jks", ".keystore"}
	// envTemplates are the environment files that a repository shares on purpose.
	envTemplates = []string{".env.example", ".env.sample", ".env.template"}
)

// envPrefix starts the name of an environment file for one setting, such as .env.local.
const envPrefix = ".env."

func secretName(segment string) bool {
	name := foldASCII(segment)
	if strings.HasPrefix(name, envPrefix) {
		return !slices.Contains(envTemplates, name)
	}
	return slices.Contains(secretNames, name) ||
		slices.ContainsFunc(secretSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) })
}

// foldASCII returns s with the ASCII letters in lower case, byte by byte, as Git folds
// case.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = lower(c)
	}
	return string(b)
}
