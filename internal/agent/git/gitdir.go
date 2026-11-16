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

package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ErrGitDirUntrusted means the Git directory holds something that the agent did not put
// there, so Git is not run against it (docs/adr/0019). Use errors.Is, and errors.As with a
// *GitDirError for what it is.
var ErrGitDirUntrusted = errors.New("the Git directory holds something the agent did not put there")

// GitDirError says what in the Git directory is not what the agent makes.
type GitDirError struct {
	// Path is the file or directory, as the agent names it.
	Path string
	// Problem says what is wrong with it.
	Problem string
}

// Error returns the path and the problem.
func (e *GitDirError) Error() string { return e.Path + " " + e.Problem }

// Is makes errors.Is(err, ErrGitDirUntrusted) true.
func (e *GitDirError) Is(target error) bool { return target == ErrGitDirUntrusted }

// maxConfigSize is the most that a config the agent wrote can be: a handful of lines.
const maxConfigSize = 64 << 10

// A kind says what a top-level entry of the Git directory has to be.
type kind int

const (
	kindFile kind = iota
	kindDir
	// kindEmptyDir is a directory that has nothing in it: where a hook, an attributes file or
	// a list of grafts would be.
	kindEmptyDir
)

// gitDirEntries are the entries at the top of a Git directory that the agent makes. They
// are what init with an empty template and the subcommands the Runner allows write. Anything
// else is refused, because a file that a later Git reads, and that this does not know of, is
// the case an allowlist is for. A name is added when Git turns out to write one.
var gitDirEntries = map[string]kind{
	"HEAD": kindFile, "config": kindFile, "index": kindFile, "FETCH_HEAD": kindFile, "ORIG_HEAD": kindFile,
	"COMMIT_EDITMSG": kindFile, "MERGE_HEAD": kindFile, "MERGE_MSG": kindFile, "MERGE_MODE": kindFile, "AUTO_MERGE": kindFile,
	"SQUASH_MSG": kindFile,
	"objects":    kindDir, "refs": kindDir, "logs": kindDir,
	"hooks": kindEmptyDir, "info": kindEmptyDir,
}

// lockSuffix ends the name of the lock file that Git makes next to a file it is changing. Only
// a lock of a file that the agent makes, and of packed-refs, which a command that removes a ref
// locks, is accepted: a name of another kind that ends in it is not a lock, and is refused like
// any other name.
const lockSuffix = ".lock"

// notThere are files inside the object directory that make Git read objects from another
// directory, which can be one the application writes.
var notThere = []string{"objects/info/alternates", "objects/info/http-alternates"}

// VerifyGitDir returns a *GitDirError when the Git directory holds something that the agent
// did not put there, and nil when it is what the agent makes or is not there yet. It looks
// without following a link, and refuses a link in the entries at the top.
//
// It is a check of the files that Git reads before it does anything and that can make it run
// a program, read configuration from another place or see another history: the config, the
// entries at the top, hooks, info and the alternates of the object store. It does not look at
// the objects or the refs. A change between the look and the start of Git is not ruled out;
// it takes a writer that can open the directory, which is mode 0700 and the agent's.
func VerifyGitDir(gitDir, workTree string) error {
	root, err := os.OpenRoot(gitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := checkEntry(root, gitDir, entry.Name()); err != nil {
			return err
		}
	}
	for _, name := range notThere {
		if _, err := root.Lstat(name); err == nil {
			return &GitDirError{Path: filepath.Join(gitDir, filepath.FromSlash(name)),
				Problem: "makes Git read objects from another directory, which the agent never does"}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return checkConfig(root, gitDir, workTree)
}

// checkEntry checks one entry at the top of the Git directory.
func checkEntry(root *os.Root, gitDir, name string) error {
	path := filepath.Join(gitDir, name)
	want, known := gitDirEntries[name]
	if topLockName(name) {
		want, known = kindFile, true
	}
	if !known {
		return &GitDirError{Path: path, Problem: "is not a file that the agent makes in a Git directory"}
	}
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		// Another command removed it since the directory was listed: a lock is made and removed
		// while a command runs, and the Runner is safe for concurrent use.
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return &GitDirError{Path: path, Problem: "is a link, and the agent does not follow links in its own directories"}
	}
	switch want {
	case kindFile:
		if !info.Mode().IsRegular() {
			return &GitDirError{Path: path, Problem: "is not a file"}
		}
	case kindDir, kindEmptyDir:
		if !info.IsDir() {
			return &GitDirError{Path: path, Problem: "is not a directory"}
		}
	}
	if want != kindEmptyDir {
		return nil
	}
	dir, err := root.OpenRoot(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	inside, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}
	if len(inside) > 0 {
		return &GitDirError{Path: path, Problem: fmt.Sprintf("holds %s, and the agent keeps it empty", inside[0].Name())}
	}
	return nil
}

// configSection is a section header without a subsection or anything after it.
var configSection = regexp.MustCompile(`^\[([A-Za-z0-9-]+)\]$`)

// configKeys are the settings that the agent's own init writes, with what each may be. A
// repository config with any other key is refused: it is where url.<base>.insteadOf, a key
// scoped to a URL, http.sslCAPath, core.sshCommand and an include would be, and the Runner
// cannot neutralise them all from the command line (docs/adr/0013).
func configKeys(workTree string) map[string]func(string) bool {
	// Git writes core.worktree as the real path, so a mount path that has a link in it is
	// written without the link.
	resolved, err := filepath.EvalSymlinks(workTree)
	if err != nil {
		resolved = workTree
	}
	oneOf := func(values ...string) func(string) bool {
		return func(v string) bool { return slices.Contains(values, v) }
	}
	boolean := oneOf("true", "false")
	return map[string]func(string) bool{
		"core.repositoryformatversion": oneOf("0", "1"),
		"core.filemode":                boolean,
		"core.bare":                    oneOf("false"),
		"core.logallrefupdates":        boolean,
		"core.worktree":                func(v string) bool { return unquote(v) == resolved },
		// What init finds out about the volume.
		"core.symlinks":           boolean,
		"core.ignorecase":         boolean,
		"core.precomposeunicode":  boolean,
		"extensions.objectformat": oneOf("sha1", "sha256"),
	}
}

// checkConfig reads the config of the Git directory and refuses what init does not write. It
// reads a file that is a regular file and small, and it does not follow includes: an include is
// a section, and a section other than core and extensions is refused.
func checkConfig(root *os.Root, gitDir, workTree string) error {
	path := filepath.Join(gitDir, "config")
	info, err := root.Lstat("config")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() > maxConfigSize {
		return &GitDirError{Path: path, Problem: "is larger than a config that the agent writes"}
	}
	data, err := root.ReadFile("config")
	if err != nil {
		return err
	}

	allowed := configKeys(workTree)
	section := ""
	for number, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		bad := func(format string, args ...any) error {
			return &GitDirError{Path: path, Problem: fmt.Sprintf("line %d %s", number+1, fmt.Sprintf(format, args...))}
		}
		if line[0] == '[' {
			m := configSection.FindStringSubmatch(line)
			if m == nil {
				return bad("is a section header that the agent does not write")
			}
			if section = strings.ToLower(m[1]); section != "core" && section != "extensions" {
				return bad("starts the section [%s], which the agent does not write", section)
			}
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		if section == "" || !hasValue || strings.HasSuffix(line, `\`) {
			return bad("is not a setting that the agent writes")
		}
		name := section + "." + strings.ToLower(strings.TrimSpace(key))
		valid, known := allowed[name]
		if !known {
			return bad("sets %s, which the agent does not write", name)
		}
		if !valid(strings.TrimSpace(value)) {
			return bad("sets %s to %q, which is not what the agent writes", name, strings.TrimSpace(value))
		}
	}
	return nil
}

// unquote reads a value the way Git writes one that has a quote or a backslash in it: between
// double quotes, with the escapes \\ and \". A value that is not quoted is as it is.
func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(v[1 : len(v)-1])
}
