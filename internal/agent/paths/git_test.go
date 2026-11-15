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
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The test runs plain Git, and not the Runner of the agent: it asks Git which files of
// a directory each pattern ignores, as the only line of an exclude file, and holds the
// matcher to the same answer. The meaning of a pattern is Git's, so Git is the oracle.

// corpusFiles are the files of the directory, each a name that some pattern is about.
// corpusLinks are symbolic links, which Git lists as files: a link to a directory is not
// a directory to a pattern.
var (
	corpusFiles = []string{
		pathTmp, pathNestedTmp, pathTmpDir, pathUpperTmp, "a.TMP", "a.tmp.txt", nameLogs, pathLogsInside,
		"logs2/x", nameJSON, pathJSONInA, pathDeepJSON, pathAxJSON, "tree/deep/er/f", "tree/er/f", "abx/c",
		"abx/y/c", "ab/c", "x/abc/d", "xab/c", nameHash, "!bang", nameSpace, "sp", "[x", nameClass1,
		"a1.txt", nameStar, "aXb", "a?b", `q\w`, nameEnv, "a/.env", ".env.example", "id_rsa", "id_rsa.pub",
		nameAccent, nameAccentUpper, "e.txt", pathLower, pathUpper, pathB, "B.txt", "z.txt", "Z.txt", "_.txt",
		"^.txt", "-", "]", "a-b", "tab\there", "UPPER/x", "upper/y",
	}
	corpusLinks = map[string]string{"linklogs": "logs2", "linkfile": nameJSON}
)

// corpusPatterns are the patterns whose meaning is compared with Git's, with case as it
// is.
var corpusPatterns = []string{
	patternTmp, patternUpperTmp, patternLogsDir, nameLogs, patternJSONInA, patternOneDirJSON,
	patternAnyDepthJSON, patternInA, patternUnderA, patternDirsInA, "tree/**/f", "tree/*/f", "tree/*/er/f",
	"*/er/*", "**/er", "**/er/", patternAnyDir, "**", "**/**", "*", "a/", nameJSON, "x/abc", "x/abc/",
	"ab*/c", "*b*/c", "linklogs/", "linklogs", "linkfile", patternNotA, patternCaretA, patternRangeAC, "[c-a].txt",
	patternUpperRange, patternRangeZa, patternFirstBracket, patternNotBracket, patternDashLast, patternDashFirst, "a[-]b",
	"[a-c-e].txt", `[a\-c].txt`, `[\[]x`, patternToBracket, "[é].txt", `\#n#`, nameHash, patternBang, nameSpace,
	`sp\ `, nameClass1, `a\[1].txt`, patternEscapedStar, nameStar, "a?b", `a\?b`, patternBackslash, nameEnv,
	".env.*", "id_*", patternOneByte, "??.txt", nameAccent, "tab?here", "tab\there", "UPPER", "upper/", "a.*",
	"*.tmp/x", "a/x.json/", "a[**]b", `[\a-c].txt`,
}

// foldPatterns are compared with Git's meaning when case is folded. They leave out an
// upper-case letter in a class or after a '\', where the two differ on purpose.
var foldPatterns = []string{
	patternTmp, patternUpperTmp, "A.tmp", patternLowerRange, patternUpperRange, patternRangeZa, patternNotA,
	"[!b-z].txt", "UPPER/", "upper", nameAccent, nameAccentUpper, "A/*.json", "X/ABC", "a/**/X.JSON", `\a.txt`,
	"?.TXT",
}

func TestPatternsMeanWhatTheyMeanToGit(t *testing.T) {
	git := newGitOracle(t)
	compare := func(patterns []string, options Options) {
		for _, pattern := range patterns {
			p, err := Compile(pattern, options)
			if err != nil {
				t.Errorf("Compile(%q) = %v", pattern, err)
				continue
			}
			ours := git.matches(p.Match)
			theirs := git.ignored(t, pattern, options.FoldCase)
			if !slices.Equal(ours, theirs) {
				t.Errorf("%q with %+v:\n  Git takes  %q\n  this takes %q", pattern, options, theirs, ours)
			}
		}
	}
	compare(corpusPatterns, Options{})
	compare(foldPatterns, Options{FoldCase: true})
}

// The one place where folded case differs from Git, which the doc of FoldCase states.
// If a release of Git changes, this says so.
func TestFoldCaseTakesAnUpperCaseLetterInAClassWhereGitTakesNothing(t *testing.T) {
	git := newGitOracle(t)
	for _, pattern := range []string{patternUpper, `\A.txt`} {
		if theirs := git.ignored(t, pattern, true); len(theirs) != 0 {
			t.Errorf("Git takes %q for %q when case is folded, and the doc of FoldCase says it takes nothing", theirs,
				pattern)
		}
		p, err := Compile(pattern, Options{FoldCase: true})
		if err != nil {
			t.Fatal(err)
		}
		if ours := git.matches(p.Match); !slices.Equal(ours, []string{pathUpper, pathLower}) {
			t.Errorf("%q takes %q when case is folded, want %s and %s", pattern, ours, pathUpper, pathLower)
		}
	}
}

// gitOracle is a directory with the corpus in it, and the Git that is asked about it.
type gitOracle struct {
	git, workTree, gitDir string
	// files are the paths of the corpus, sorted as Git sorts them.
	files []string
}

func newGitOracle(t *testing.T) *gitOracle {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed, and this test asks the real git binary")
	}
	dir := t.TempDir()
	if foldsCase(t, dir) {
		t.Skip("the temporary directory folds case, and the corpus has names that differ only by it")
	}
	o := &gitOracle{git: git, workTree: filepath.Join(dir, "tree"), gitDir: filepath.Join(dir, "git")}
	for _, file := range corpusFiles {
		name := filepath.Join(o.workTree, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range corpusLinks {
		if err := os.Symlink(target, filepath.Join(o.workTree, link)); err != nil {
			t.Fatal(err)
		}
	}
	o.run(t, "init", "--quiet", "--template=")
	o.files = files(o.run(t, "ls-files", "-z", "--others"))
	want := slices.Concat(corpusFiles, slices.Collect(maps.Keys(corpusLinks)))
	slices.Sort(want)
	if !slices.Equal(o.files, want) {
		t.Fatalf("Git lists %q in the corpus, want %q", o.files, want)
	}
	return o
}

// foldsCase reports whether a directory is on a file system that takes two names that differ only in
// case for one file, as the default one of macOS does. The corpus has such names, and so it needs a
// file system that tells them apart. One that cannot hold some of its names, as on Windows, is left
// out on the same ground.
func foldsCase(t *testing.T, dir string) bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		return true
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "PROBE"))
	return err == nil
}

// ignored returns the files of the corpus that Git ignores when the pattern is the only
// line of an exclude file.
func (o *gitOracle) ignored(t *testing.T, pattern string, foldCase bool) []string {
	t.Helper()
	file := filepath.Join(filepath.Dir(o.gitDir), "exclude")
	if err := os.WriteFile(file, []byte(gitLine(pattern)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return files(o.run(t, "-c", "core.ignoreCase="+strconv.FormatBool(foldCase), "ls-files", "-z", "--others",
		"--ignored", "--exclude-from="+file))
}

// files returns the paths that ls-files -z printed, sorted.
func files(out string) []string {
	if out == "" {
		return nil
	}
	list := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	slices.Sort(list)
	return list
}

// run runs Git on the corpus with an environment of its own, so that nothing of the
// user's or the system's configuration is read.
func (o *gitOracle) run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(o.git, slices.Concat([]string{"-c", "core.excludesFile=/dev/null"}, args)...)
	cmd.Dir = o.workTree
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "LC_ALL=C", "GIT_DIR=" + o.gitDir,
		"GIT_WORK_TREE=" + o.workTree, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// matches returns the files of the corpus that match.
func (o *gitOracle) matches(match func(string) bool) []string {
	var files []string
	for _, file := range o.files {
		if match(file) {
			files = append(files, file)
		}
	}
	return files
}

// gitLine writes a pattern as a line of an exclude file that Git reads as the same
// pattern. A file is not a list: a '#' at the start would make the line a comment, and
// Git drops the spaces at the end that are not escaped.
func gitLine(pattern string) string {
	var line strings.Builder
	if strings.HasPrefix(pattern, "#") {
		line.WriteByte('\\')
	}
	spaces := len(strings.TrimRight(pattern, " "))
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' {
			line.WriteString(pattern[i : i+2])
			i++
			continue
		}
		if i >= spaces {
			line.WriteByte('\\')
		}
		line.WriteByte(pattern[i])
	}
	return line.String()
}
