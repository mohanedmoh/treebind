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

// Package paths reads the patterns of spec.paths, which say which files of the synced
// directory the agent syncs (#43). It does not touch Git or the file system.
//
// A pattern has the syntax and the meaning of a line of a .gitignore file, so that a
// pattern in spec.paths and one in the repository mean the same: "*", "?", "[...]",
// "\" and "**". A pattern without a slash matches a name at any depth. One with a slash
// is relative to the synced directory. One that ends with a slash matches only a
// directory, and a directory that matches takes every file in it. The patterns are a
// list and not a file, so "#" and spaces at the end are not special, and there is no
// negation. Matching is on bytes, as in Git: "?" is one byte, so it does not match "é",
// which is two.
//
// Check refuses a pattern that Git would never match, or would read in a way its own
// documentation does not say, so that an exclude cannot silently do nothing.
package paths

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxPatternLength is the most characters that a pattern may have, as in the CRD.
const MaxPatternLength = 512

var (
	errEscape = errors.New(`must not end with '\' or put one before a '/'`)
	errClass  = errors.New("must close each '[' with a ']' in the same segment")
)

// Check returns the reason the agent refuses a pattern, or nil when it takes it.
func Check(pattern string) error {
	switch {
	case pattern == "":
		return errors.New("must not be empty")
	case utf8.RuneCountInString(pattern) > MaxPatternLength:
		return fmt.Errorf("must be at most %d characters", MaxPatternLength)
	case strings.ContainsRune(pattern, 0):
		return errors.New("must not contain a NUL, which no file name has")
	case strings.HasPrefix(pattern, "/"):
		return errors.New("must be relative to the synced directory")
	case strings.HasPrefix(pattern, "!"):
		return errors.New(`must not start with '!': there is no negation, and an exclude wins over an include; ` +
			`write '\!' for a name that starts with '!'`)
	}
	for segment := range strings.SplitSeq(strings.TrimSuffix(pattern, "/"), "/") {
		switch segment {
		case "..":
			return errors.New("must not contain a '..' segment")
		case "", ".":
			return errors.New("must not have an empty or '.' segment, which no path has")
		}
		if err := checkSegment(segment); err != nil {
			return err
		}
	}
	return nil
}

// checkSegment returns the reason a segment of a pattern, the text between two slashes,
// is refused. Git reads a "**" that is not a whole segment as two stars when it comes
// after other wildcards, and as "**" when it comes first, so "ab**/c" matches "abx/y/c"
// in Git. Refusing it leaves only the meaning that Git's documentation gives.
func checkSegment(segment string) error {
	for i := 0; i < len(segment); i++ {
		switch segment[i] {
		case '\\':
			if i++; i == len(segment) {
				return errEscape
			}
		case '*':
			if i+1 < len(segment) && segment[i+1] == '*' && segment != "**" {
				return errors.New("must use '**' only as a whole segment, as in a/**/b; inside a name '*' is enough")
			}
		case '[':
			end, err := classEnd(segment, i+1)
			if err != nil {
				return err
			}
			i = end
		}
	}
	return nil
}

// classEnd returns the index of the ']' that closes the class whose members start at
// s[i]. A ']' that comes first, after the '!' or '^' of a negated class, is a member.
// An unescaped '[' is refused in a class, which leaves out the classes like [:alpha:]
// and keeps where a class ends the same however a range in it is read.
func classEnd(s string, i int) (int, error) {
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		i++
	}
	if i < len(s) && s[i] == ']' {
		i++
	}
	for ; i < len(s); i++ {
		switch s[i] {
		case ']':
			return i, nil
		case '\\':
			if i++; i == len(s) {
				return 0, errEscape
			}
		case '[':
			return 0, errors.New(`must write '\[' for a '[' in a class; classes like [:alpha:] are not supported`)
		}
	}
	return 0, errClass
}

// Options change how a pattern matches.
type Options struct {
	// FoldCase makes an ASCII letter match in either case, as Git does with
	// core.ignoreCase, which it sets on a volume that folds case. In one thing it differs
	// from Git: there an upper-case letter after a '\' or in a class matches nothing,
	// and here it matches in either case, like every other letter.
	FoldCase bool
}

// Pattern is a pattern that Check takes, ready to match paths. It does not change once
// it is compiled, so it is safe for concurrent use.
type Pattern struct {
	tokens []token
	// anchored is a pattern with a slash before its end. It is matched from the top of
	// the synced directory. One without is matched against the name of each directory on
	// a path and of the file.
	anchored bool
	// dirOnly is a pattern that ended with a slash, which matches only a directory.
	dirOnly bool
}

// kind is what a token of a pattern matches.
type kind uint8

const (
	// one is one byte of the token's set.
	one kind = iota
	// many is any run of bytes of the token's set, the empty one among them.
	many
	// dirs is "**/": nothing, or any run that ends with a slash, which is any number of
	// directories.
	dirs
)

type token struct {
	kind kind
	set  byteSet
}

// byteSet is a set of bytes.
type byteSet [4]uint64

func (s *byteSet) add(b byte) { s[b>>6] |= 1 << (b & 63) }

func (s *byteSet) has(b byte) bool { return s[b>>6]&(1<<(b&63)) != 0 }

func setOf(in func(byte) bool) byteSet {
	var s byteSet
	for b := range 256 {
		if in(byte(b)) {
			s.add(byte(b))
		}
	}
	return s
}

var (
	notSlash = setOf(func(b byte) bool { return b != '/' })
	slash    = setOf(func(b byte) bool { return b == '/' })
)

// Compile checks a pattern and makes it ready to match.
func Compile(pattern string, options Options) (*Pattern, error) {
	if err := Check(pattern); err != nil {
		return nil, err
	}
	body, dirOnly := strings.CutSuffix(pattern, "/")
	p := &Pattern{anchored: strings.Contains(body, "/"), dirOnly: dirOnly}
	segments := strings.Split(body, "/")
	for i, segment := range segments {
		last := i == len(segments)-1
		switch {
		case segment != "**":
			p.tokens = appendSegment(p.tokens, segment, options.FoldCase)
		case last:
			// "**" alone, or at the end: the same as "*", because a directory that matches
			// takes everything in it.
			p.tokens = append(p.tokens, token{kind: many, set: notSlash})
		default:
			// The slash after "**" is part of it, because "**/" may match nothing.
			p.tokens = append(p.tokens, token{kind: dirs})
			continue
		}
		if !last {
			p.tokens = append(p.tokens, token{kind: one, set: slash})
		}
	}
	return p, nil
}

// appendSegment appends the tokens of a segment that Check took.
func appendSegment(tokens []token, segment string, foldCase bool) []token {
	for i := 0; i < len(segment); i++ {
		switch c := segment[i]; c {
		case '*':
			tokens = append(tokens, token{kind: many, set: notSlash})
		case '?':
			tokens = append(tokens, token{kind: one, set: notSlash})
		case '[':
			var set byteSet
			set, i = class(segment, i+1, foldCase)
			tokens = append(tokens, token{kind: one, set: set})
		case '\\':
			i++
			tokens = append(tokens, token{kind: one, set: literal(segment[i], foldCase)})
		default:
			tokens = append(tokens, token{kind: one, set: literal(c, foldCase)})
		}
	}
	return tokens
}

// literal returns the set of one byte, and of the same letter in the other case when
// case is folded.
func literal(c byte, foldCase bool) byteSet {
	var s byteSet
	s.add(c)
	if foldCase {
		s.add(lower(c))
		s.add(upper(c))
	}
	return s
}

// class reads a class that Check took, whose members start at s[i], as Git's wildmatch
// reads it, and returns the bytes it matches and the index of its closing ']'. A '-'
// between two members is a range, and after a range or at the start it is a member. A
// class never matches a slash.
func class(s string, i int, foldCase bool) (byteSet, int) {
	negated := s[i] == '!' || s[i] == '^'
	if negated {
		i++
	}
	var members byteSet
	// prev is the member before, which a '-' makes the start of a range, or -1.
	prev := -1
	for first := true; first || s[i] != ']'; i++ {
		first = false
		c := s[i]
		switch {
		case c == '\\':
			i++
			members.add(s[i])
			prev = int(s[i])
		// The class is closed, so a member is always followed by something.
		case c == '-' && prev >= 0 && s[i+1] != ']':
			i++
			hi := s[i]
			if hi == '\\' {
				i++
				hi = s[i]
			}
			for b := prev; b <= int(hi); b++ {
				members.add(byte(b))
			}
			prev = -1
		default:
			members.add(c)
			prev = int(c)
		}
	}
	set := setOf(func(b byte) bool {
		in := members.has(b) || foldCase && (members.has(lower(b)) || members.has(upper(b)))
		return in != negated && b != '/'
	})
	return set, i
}

func lower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func upper(c byte) byte {
	if 'a' <= c && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// Match reports whether the pattern takes the file at path: the file itself, or a
// directory that holds it. The path is relative to the synced directory and has
// slashes, as Git lists it: none at its start or end, and no empty segment.
//
// The answer is worked out in a table of the tokens by the bytes of the path, filled
// from the end, so it takes time in proportion to their product whatever the pattern
// is. A pattern comes from whoever can edit the Treebind.
func (p *Pattern) Match(path string) bool {
	n := len(path)
	// rest[j] says whether the tokens after the one at hand match path[j:] up to the end
	// of a name: the end of the path, which names the file, or a slash, which ends the
	// name of a directory. With no tokens left, that is where a name ends.
	rest := make([]bool, n+1)
	for j := range n {
		rest[j] = path[j] == '/'
	}
	rest[n] = !p.dirOnly
	here := make([]bool, n+1)
	for i := len(p.tokens) - 1; i >= 0; i-- {
		t := &p.tokens[i]
		// At the end of the path, only a token that may match nothing matches.
		here[n] = t.kind != one && rest[n]
		switch t.kind {
		case one:
			for j := n - 1; j >= 0; j-- {
				here[j] = t.set.has(path[j]) && rest[j+1]
			}
		case many:
			for j := n - 1; j >= 0; j-- {
				here[j] = rest[j] || (t.set.has(path[j]) && here[j+1])
			}
		case dirs:
			// Whether a slash at j or after it ends a run that the rest matches from.
			dir := false
			for j := n - 1; j >= 0; j-- {
				dir = dir || (path[j] == '/' && rest[j+1])
				here[j] = rest[j] || dir
			}
		}
		here, rest = rest, here
	}
	if rest[0] {
		return true
	}
	// A pattern without a slash also matches from the start of every other name. It has
	// no token that matches a slash, so it cannot run into the next name.
	if !p.anchored {
		for j := range n {
			if path[j] == '/' && rest[j+1] {
				return true
			}
		}
	}
	return false
}
