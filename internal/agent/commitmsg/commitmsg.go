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

// Package commitmsg turns the commit settings of a Treebind into what Git is given: the message of a commit, from
// a template and the number of files it holds, and a check of the fixed author that every commit of the agent has
// (FR-3). Both are pure: they run no Git, read no file and use only the standard library, so what the agent
// commits is decided, and held by tests, before anything commits.
//
// The agent passes the author to Git through its environment and the message through an argument. Git drops a newline,
// '<' and '>' from a name or an email when it writes the author, and a space and . , : ; " ' \ from its start and its
// end (a period only on some releases: Git 2.34.1 removes it and 2.56.1, the Git of the agent image, keeps it), so a commit
// would carry another author than the one that was set. No argument of a command can hold a NUL byte,
// so the call would fail with a message that says nothing of the setting. A test holds each to a real Git. The checks
// here refuse the values first, and name the setting.
package commitmsg

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"
)

// DefaultTemplate is the message of a commit when the Treebind has no template of its own.
const DefaultTemplate = "treebind: sync {{.Count}} file(s)"

// DefaultAuthorName and DefaultAuthorEmail are the author of every commit when the Treebind names none. The
// domain of the email is one that cannot exist (RFC 2606), so that no mail goes to anyone and no account of a
// host is taken for the agent. A commit by it is easy to tell from one by a person.
const (
	DefaultAuthorName  = "treebind-bot"
	DefaultAuthorEmail = "treebind-bot@treebind.invalid"
)

// MaxMessageBytes is the longest output that a template may have. A commit message is a few lines, and one longer than
// this does not fit in an argument of a command on every system. It also stops a template that loops for a very long
// time, as `range` over a large number does, from building a message until the agent has no memory left.
const MaxMessageBytes = 64 << 10

// Data is what a template can use. A template that names anything else is refused.
type Data struct {
	// Count is how many files the commit holds.
	Count int
}

// Render writes the message of a commit from a template. The template is the one of text/template with only its
// built-in functions, so it cannot run a program or read a file, and it may span several lines: the first is the
// subject. The whitespace around the message is trimmed and the blank lines inside it are kept.
//
// It is an error when the template does not parse, names a field that Data does not have, renders to nothing, renders
// more than MaxMessageBytes, or renders a NUL byte, which no argument of a command can hold. The error says that the
// commit message template is wrong, and why.
func Render(tmpl string, data Data) (string, error) {
	t, err := template.New("commit").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("the commit message template is not valid: %w", err)
	}
	var out limitedBuilder
	if err := t.Execute(&out, data); err != nil {
		if errors.Is(err, errTooLong) {
			return "", fmt.Errorf("the commit message template renders more than %d bytes", MaxMessageBytes)
		}
		return "", fmt.Errorf("the commit message template cannot be rendered: %w", err)
	}
	message := strings.TrimSpace(out.String())
	switch {
	case message == "":
		return "", errors.New("the commit message template renders to an empty message")
	case strings.ContainsRune(message, 0):
		return "", errors.New("the commit message template renders a NUL byte")
	}
	return message, nil
}

var errTooLong = errors.New("too long")

// limitedBuilder collects what a template writes, and refuses what is more than MaxMessageBytes, so that a template that
// loops stops at the limit and does not run to its end.
type limitedBuilder struct{ b strings.Builder }

func (l *limitedBuilder) Write(p []byte) (int, error) {
	if l.b.Len()+len(p) > MaxMessageBytes {
		return 0, errTooLong
	}
	return l.b.Write(p)
}

func (l *limitedBuilder) String() string { return l.b.String() }

// ValidateAuthor checks the name and the email that every commit of the agent has as its author. The name must have
// something in it, no control character, and no '<' or '>', which Git takes for the start and the end of the address.
// Git keeps most control characters and drops a newline, but a name that has one is never meant, and one that holds a
// terminal escape sequence makes the log of the commits misleading. Neither value may start or end with a space or one
// of . , : ; " ' \, which Git removes from both, except that a period is kept by the newer releases: it is refused too,
// so that the commits say the same on every Git. The email must be one bare address, as net/mail reads it, with no
// display name, no comment and nothing around it. Both are checked, and the error names each that is wrong.
func ValidateAuthor(name, email string) error {
	return errors.Join(checkName(name), checkEmail(email))
}

// gitCrud says whether some release of Git removes a byte from the start and the end of a name and of an email when it
// writes the author: a space or a control character, and . , : ; < > " ' \. Git 2.56.1 keeps a period, and 2.34.1 does
// not, and every other byte is the same on both, which a test holds for each byte.
func gitCrud(c byte) bool { return c <= ' ' || strings.IndexByte(".,:;<>\"'\\", c) >= 0 }

// trimmedByGit returns what Git writes for a value that it trims, and whether it trims it.
func trimmedByGit(s string) (string, bool) {
	trimmed := strings.TrimFunc(s, func(r rune) bool { return r < utf8.RuneSelf && gitCrud(byte(r)) })
	return trimmed, trimmed != s
}

func checkName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("the author name is empty")
	case strings.ContainsAny(name, "<>"):
		return errors.New("the author name must not contain '<' or '>': Git reads them as the start and the end of the email")
	case strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("the author name must not contain a control character such as a newline")
	}
	if trimmed, trims := trimmedByGit(name); trims {
		return fmt.Errorf("the author name %q must not start or end with a space or one of . , : ; ' \" \\: "+
			"Git removes them (a period only in some releases), so the commits would say %q", name, trimmed)
	}
	return nil
}

func checkEmail(email string) error {
	if email == "" {
		return errors.New("the author email is empty")
	}
	address, err := mail.ParseAddress(email)
	if err != nil {
		return fmt.Errorf("the author email %q is not an email address: %w", email, err)
	}
	// ParseAddress takes "Name <a@b>", a comment and the space around an address. What it returns is the bare
	// address, so anything it dropped makes the email something else than a single bare address.
	if address.Address != email {
		return fmt.Errorf("the author email %q must be one bare address such as %s, with no name, comment or space", email, address.Address)
	}
	if trimmed, trims := trimmedByGit(email); trims {
		return fmt.Errorf("the author email %q must not start or end with a space or one of . , : ; ' \" \\: "+
			"Git removes them (a period only in some releases), so the commits would say %q", email, trimmed)
	}
	return nil
}
