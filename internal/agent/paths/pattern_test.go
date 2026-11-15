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
	"strings"
	"testing"
)

// Strings that the tests repeat. A name is used both as a pattern and as a path.
const (
	patternTmp          = "*.tmp"
	patternUpperTmp     = "*.TMP"
	patternLogsDir      = "logs/"
	nameJSON            = "x.json"
	patternJSONInA      = "a/*.json"
	patternOneDirJSON   = "*/x.json"
	patternAnyDepthJSON = "**/x.json"
	patternUnderA       = "a/**/x.json"
	patternInA          = "a/**"
	patternDirsInA      = "a/**/"
	patternAnyDir       = "**/"
	patternDirAB        = "a/b/"
	patternNotA         = "[!a].tmp"
	patternCaretA       = "[^a].tmp"
	patternFirstBracket = "[]a]"
	patternNotBracket   = "[!]a]"
	patternDashFirst    = "[-a]"
	patternDashLast     = "[a-]"
	patternRangeAC      = "[a-c].txt"
	patternLowerRange   = "[a-z].txt"
	patternUpperRange   = "[A-Z].txt"
	patternRangeZa      = "[Z-a].txt"
	patternUpper        = "[A].txt"
	nameClass1          = "a[1].txt"
	patternEscapedStar  = `a\*b`
	patternOneByte      = "?.txt"
	nameHash            = "#n#"
	patternBang         = `\!bang`
	patternBackslash    = `q\\w`
	patternEscapedDash  = `[a\-c]`
	patternToBracket    = `[a-\]]`
	nameSpace           = "sp "

	pathTmp         = "a.tmp"
	pathNestedTmp   = "b/c.tmp"
	pathTmpDir      = "dir.tmp/x"
	pathUpperTmp    = "A.TMP"
	nameLogs        = "logs"
	pathLogsInside  = "a/logs/x"
	pathJSONInA     = "a/x.json"
	pathDeepJSON    = "a/b/x.json"
	pathAxJSON      = "ax.json"
	pathDeep        = "a/b/c"
	pathAC          = "a/c"
	pathAB          = "a/b"
	pathLower       = "a.txt"
	pathUpper       = "A.txt"
	pathB           = "b.txt"
	nameAccent      = "é.txt"
	nameAccentUpper = "É.txt"
	nameStar        = "a*b"
	nameEnv         = ".env"

	wantRelative = "must be relative"
	wantParent   = "'..' segment"
	wantSegment  = "empty or '.' segment"
	wantEscape   = `must not end with '\'`
	wantClose    = "must close each '['"
	wantInClass  = `'\[' for a '[' in a class`
	wantStars    = "'**' only as a whole segment"
)

func TestCheckTakes(t *testing.T) {
	for _, pattern := range []string{
		patternTmp, patternLogsDir, patternJSONInA, "**/x", patternInA, "a/**/b", "**", patternAnyDir, patternDirsInA,
		patternNotA, patternCaretA, patternFirstBracket, patternNotBracket, "[a-z]", `[a\]b]`, `[\[]`, patternDashLast,
		patternDashFirst, "a[-]b", patternBang, `\#n#`, nameHash, nameSpace, `a\ `, `x\*y`, `\**`, `*\**`,
		patternBackslash, nameEnv, ".env.*", patternDirAB, "?", "é", "a b", "a\tb", "a\nb",
		strings.Repeat("a", MaxPatternLength), strings.Repeat("é", MaxPatternLength), "..x", "x..", "a..b", ".x", "x.", "a[**]b",
	} {
		if err := Check(pattern); err != nil {
			t.Errorf("Check(%q) = %v, want nil", pattern, err)
		}
	}
}

func TestCheckRefuses(t *testing.T) {
	tests := []struct{ pattern, want string }{
		{"", "must not be empty"},
		{strings.Repeat("a", MaxPatternLength+1), "at most 512 characters"},
		{strings.Repeat("é", MaxPatternLength+1), "at most 512 characters"},
		{"a\x00b", "NUL"},
		{"/etc/**", wantRelative},
		{"/", wantRelative},
		{"!keep.json", "there is no negation"},
		{"..", wantParent},
		{"../a", wantParent},
		{"a/../b", wantParent},
		{"a/..", wantParent},
		{"../", wantParent},
		{".", wantSegment},
		{"./a.tmp", wantSegment},
		{"a/./b", wantSegment},
		{"a/.", wantSegment},
		{"a//b", wantSegment},
		{"a//", wantSegment},
		{`b\`, wantEscape},
		{`a\/b`, wantEscape},
		{`a\/`, wantEscape},
		{`[a\`, wantEscape},
		{`[a\/]`, wantEscape},
		{"[x", wantClose},
		{"a[", wantClose},
		{"a[b/c]", wantClose},
		{"[]", wantClose},
		{"[!]", wantClose},
		{"[^]", wantClose},
		{"[!", wantClose},
		{"[a[b]", wantInClass},
		{"[[]", wantInClass},
		{"[[:alpha:]]", wantInClass},
		{"[a-[:alpha:]]", wantInClass},
		{"ab**/c", wantStars},
		{"a**", wantStars},
		{"**a", wantStars},
		{"***", wantStars},
		{"a/***/b", wantStars},
		{`\***`, wantStars},
	}
	for _, tt := range tests {
		err := Check(tt.pattern)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Check(%q) = %v, want an error that says %q", tt.pattern, err, tt.want)
			continue
		}
		if _, compileErr := Compile(tt.pattern, Options{}); compileErr == nil || compileErr.Error() != err.Error() {
			t.Errorf("Compile(%q) = %v, want the error of Check: %v", tt.pattern, compileErr, err)
		}
	}
}

// matchTest is a pattern, a path, and whether the pattern takes the path.
type matchTest struct {
	pattern, path string
	want          bool
}

func runMatchTests(t *testing.T, options Options, tests []matchTest) {
	t.Helper()
	for _, tt := range tests {
		p, err := Compile(tt.pattern, options)
		if err != nil {
			t.Fatalf("Compile(%q) = %v", tt.pattern, err)
		}
		if got := p.Match(tt.path); got != tt.want {
			t.Errorf("%q with %+v matches %q = %v, want %v", tt.pattern, options, tt.path, got, tt.want)
		}
	}
}

// The meaning of each part of a pattern. The test against Git holds all of it to what
// Git does; these say what that is.
func TestMatch(t *testing.T) {
	runMatchTests(t, Options{}, []matchTest{
		// Without a slash: a name at any depth, the file's or a directory's.
		{patternTmp, pathTmp, true},
		{patternTmp, pathNestedTmp, true},
		{patternTmp, pathTmpDir, true},
		{patternTmp, "a.tmp.txt", false},
		{patternTmp, pathUpperTmp, false},
		{nameJSON, pathDeepJSON, true},
		{nameJSON, pathAxJSON, false},
		// A trailing slash: only a directory.
		{patternLogsDir, pathLogsInside, true},
		{patternLogsDir, nameLogs, false},
		{patternLogsDir, "a/logs", false},
		{nameLogs, nameLogs, true},
		{nameLogs, pathLogsInside, true},
		{nameLogs, "logs2/x", false},
		// With a slash: from the top of the synced directory.
		{patternJSONInA, pathJSONInA, true},
		{patternJSONInA, pathDeepJSON, false},
		{patternJSONInA, "b/a/x.json", false},
		{patternOneDirJSON, pathJSONInA, true},
		{patternOneDirJSON, nameJSON, false},
		{"a/*", pathDeep, true},
		{patternDirAB, pathDeep, true},
		{patternDirAB, pathAB, false},
		{patternDirAB, "x/a/b/c", false},
		// "**" as a segment of its own.
		{patternAnyDepthJSON, nameJSON, true},
		{patternAnyDepthJSON, pathDeepJSON, true},
		{patternAnyDepthJSON, "a/bx.json", false},
		{patternInA, "a/x", true},
		{patternInA, "a/b/x", true},
		{patternInA, "a", false},
		{patternInA, "ab/x", false},
		{patternUnderA, pathJSONInA, true},
		{patternUnderA, "a/b/c/x.json", true},
		{patternUnderA, "a/b/c/y.json", false},
		{patternUnderA, pathAxJSON, false},
		{patternDirsInA, pathDeep, true},
		{patternDirsInA, pathAC, false},
		{patternAnyDir, pathAC, true},
		{patternAnyDir, "c", false},
		{"**", "c", true},
		{"**/**", "a/b", true},
		// "*" and "?" never match a slash, and "?" is one byte.
		{"a*c", "abbc", true},
		{"a*c", "ab/c", false},
		{"a?c", "abc", true},
		{"a?c", pathAC, false},
		{patternOneByte, "e.txt", true},
		{patternOneByte, nameAccent, false},
		{"??.txt", nameAccent, true},
		// Classes: negation with '!' or '^', ranges, a ']' that comes first, escapes.
		{patternNotA, pathNestedTmp, true},
		{patternNotA, pathTmp, false},
		{patternCaretA, pathTmp, false},
		{patternRangeAC, pathB, true},
		{patternRangeAC, "d.txt", false},
		{"[c-a].txt", pathB, false},
		{patternFirstBracket, "]", true},
		{patternFirstBracket, "a", true},
		{patternNotBracket, "b", true},
		{patternNotBracket, "]", false},
		{patternDashLast, "-", true},
		{patternDashFirst, "-", true},
		{"[a-c-e]", "-", true},
		{"[a-c-e]", "d", false},
		{patternEscapedDash, "b", false},
		{patternEscapedDash, "-", true},
		{`[\[]`, "[", true},
		{patternToBracket, "]", false},
		{"a[!b]c/d", "axc/d", true},
		{"a[!b]c/d", "a/c/d", false},
		{"a?c/d", "a/c/d", false},
		{"a*c/d", "ab/c/d", false},
		{"[é]", "é", false},
		// Escapes, and what is plain in a list.
		{patternEscapedStar, nameStar, true},
		{patternEscapedStar, "aXb", false},
		{nameClass1, "a1.txt", true},
		{nameClass1, nameClass1, false},
		{`a\[1].txt`, nameClass1, true},
		{patternBang, "!bang", true},
		{nameHash, nameHash, true},
		{nameSpace, nameSpace, true},
		{nameSpace, "sp", false},
		{patternBackslash, `q\w`, true},
	})
}

func TestMatchFoldsCase(t *testing.T) {
	runMatchTests(t, Options{FoldCase: true}, []matchTest{
		{patternTmp, pathUpperTmp, true},
		{patternUpperTmp, pathTmp, true},
		{patternLowerRange, pathUpper, true},
		{patternUpperRange, pathLower, true},
		{patternNotA, "A.tmp", false},
		{patternRangeZa, "z.txt", true},
		{patternRangeZa, "_.txt", true},
		{nameAccent, nameAccentUpper, false},
		{"1.txt", "1.TXT", true},
		// Where Git matches nothing, an upper-case letter in a class or after a '\'
		// matches in either case.
		{patternUpper, pathLower, true},
		{patternUpper, pathUpper, true},
		{`\A.txt`, pathLower, true},
		{"[!A].txt", pathLower, false},
	})
	runMatchTests(t, Options{}, []matchTest{
		{patternUpper, pathLower, false},
		{patternUpper, pathUpper, true},
		{patternLowerRange, pathUpper, false},
	})
}

// A pattern comes from whoever can edit the Treebind, and a matcher that backtracks
// takes time that grows as a power of the number of stars. These finish at once here.
func TestMatchDoesNotBacktrack(t *testing.T) {
	name := strings.Repeat("a", 4096)
	stars := strings.Repeat("*a", 250) + "*b"
	deep := strings.Repeat("**/a/", 100) + "b"
	runMatchTests(t, Options{}, []matchTest{
		{stars, name, false},
		{stars, name + "b", true},
		{deep, strings.Repeat("a/", 2000) + "c", false},
		{deep, strings.Repeat("a/", 2000) + "b", true},
	})
}

// Compile reads a pattern a second time after Check has, and relies on what Check took. This holds
// the two together: whatever the pattern, Compile agrees with Check, and a pattern that is taken
// matches or does not match without a panic. The seeds run as ordinary tests, and
// go test -fuzz=FuzzCompile -fuzztime=1m ./internal/agent/paths/ looks for more.
func FuzzCompile(f *testing.F) {
	for _, seed := range []string{
		patternTmp, patternLogsDir, patternAnyDepthJSON, patternUnderA, patternAnyDir, patternNotA, patternFirstBracket,
		patternNotBracket, patternDashFirst, patternDashLast, patternRangeAC, patternUpper, nameClass1, patternEscapedStar,
		patternBackslash, patternEscapedDash, patternToBracket, `[\`, `[a`, `[`, `\`, "a/**b", "[[:alpha:]]", "[]", "[!]", "[^]", "**", "**/**/**",
	} {
		f.Add(seed)
	}
	paths := []string{"a", pathAB, "a/b/c", "a.tmp", "dir.tmp/x", "é.txt", "[", "]", "\\", "-", "a/\\/b"}
	f.Fuzz(func(t *testing.T, pattern string) {
		checked := Check(pattern)
		for _, fold := range []bool{false, true} {
			p, err := Compile(pattern, Options{FoldCase: fold})
			if (checked == nil) != (err == nil) {
				t.Fatalf("Check(%q) = %v and Compile(%q) = %v", pattern, checked, pattern, err)
			}
			if err != nil {
				continue
			}
			for _, path := range paths {
				p.Match(path)
			}
		}
	})
}
