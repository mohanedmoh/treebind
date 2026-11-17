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

package commitmsg

import (
	"strings"
	"testing"
)

// What the tests repeat.
const (
	goodName  = "treebind-bot"
	goodEmail = "bot@example.org"
	shortName = "bot"

	// Emails that have more than the address.
	displayNameEmail = "Bot <bot@example.org>"
	bracketedEmail   = "<bot@example.org>"
	bareName         = "treebind"

	// What an error says: the setting that is wrong, and why.
	fieldName    = "name"
	fieldEmail   = "email"
	wantEmpty    = "empty"
	wantTemplate = "commit message template"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		data Data
		want string
	}{
		{"the default template", DefaultTemplate, Data{Count: 3}, "treebind: sync 3 file(s)"},
		{"the default template for one file", DefaultTemplate, Data{Count: 1}, "treebind: sync 1 file(s)"},
		{"a count of nothing is a number", "sync {{.Count}}", Data{}, "sync 0"},
		{
			"several lines: the blank line inside is kept and the trailing newline is trimmed",
			"sync {{.Count}}\n\nby treebind\n", Data{Count: 1}, "sync 1\n\nby treebind",
		},
		{"whitespace around the message is trimmed", "  \n\t sync {{.Count}} \n\n  ", Data{Count: 2}, "sync 2"},
		{"the lines inside keep their own indentation", "sync\n  {{.Count}} files\n", Data{Count: 4}, "sync\n  4 files"},
		{"a function of text/template works", `{{printf "%03d" .Count}} files`, Data{Count: 7}, "007 files"},
		{"a condition", "{{if gt .Count 1}}files{{else}}file{{end}}", Data{Count: 1}, "file"},
		{"text that is not ASCII", "synchronisé {{.Count}} fichiers — ✓", Data{Count: 5}, "synchronisé 5 fichiers — ✓"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.tmpl, tt.data)
			if err != nil || got != tt.want {
				t.Errorf("Render(%q, %+v) = %q, %v, want %q", tt.tmpl, tt.data, got, err, tt.want)
			}
		})
	}
}

func TestRenderRefusesWhatCannotBeACommitMessage(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		data Data
		want string // what the error says
	}{
		{"a field that Data does not have", "{{.Nope}}", Data{}, wantTemplate},
		{"a template that does not parse", "{{", Data{}, wantTemplate},
		{"an action that is not closed", "sync {{.Count", Data{}, wantTemplate},
		{"nothing but whitespace", "  \n", Data{}, wantEmpty},
		{"no template at all", "", Data{}, wantEmpty},
		{"a template that renders to nothing", "{{if .Count}}sync{{end}}", Data{}, wantEmpty},
		{"a field of a field that a number does not have", "{{.Count.Foo}}", Data{Count: 1}, wantTemplate},
		{"a function that does not exist", "{{shell .Count}}", Data{}, wantTemplate},
		{"a template that is not defined", `{{template "other"}}`, Data{}, wantTemplate},
		{"a NUL byte, which no argument of a command can hold", "sync\x00{{.Count}}", Data{Count: 1}, "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.tmpl, tt.data)
			if err == nil {
				t.Fatalf("Render(%q, %+v) = %q, want an error", tt.tmpl, tt.data, got)
			}
			if got != "" {
				t.Errorf("Render returned %q with its error, want nothing", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to say %q", err, tt.want)
			}
		})
	}
}

func TestValidateAuthorAcceptsAFixedAuthor(t *testing.T) {
	tests := []struct{ name, email string }{
		{"treebind-bot", goodEmail},
		{"Treebind Bot", goodEmail},
		{"Zoë Ünï ✓", "zoe@example.org"},
		{shortName, "bot+treebind@example.org"},
		{shortName, "bot@localhost"},
		{"x", "a.b-c_d@sub.example.org"},
	}
	for _, tt := range tests {
		if err := ValidateAuthor(tt.name, tt.email); err != nil {
			t.Errorf("ValidateAuthor(%q, %q) = %v, want nil", tt.name, tt.email, err)
		}
	}
}

func TestValidateAuthorRefusesWhatGitWouldMisreadOrDrop(t *testing.T) {
	tests := []struct {
		what        string
		name, email string
		want        string // fieldName or fieldEmail: which of them the error names
	}{
		{"an empty name", "", goodEmail, fieldName},
		{"a name of nothing but spaces", "   ", goodEmail, fieldName},
		{"a name with a newline", "tree\nbind", goodEmail, fieldName},
		{"a name that ends with a newline", "treebind\n", goodEmail, fieldName},
		{"a name with a carriage return", "tree\rbind", goodEmail, fieldName},
		{"a name with <", "tree<bind", goodEmail, fieldName},
		{"a name with >", "tree>bind", goodEmail, fieldName},
		{"a name that holds an address", "bot <bot@example.org>", goodEmail, fieldName},
		{"a name with a NUL", "tree\x00bind", goodEmail, fieldName},
		{"a name with a tab", "tree\tbind", goodEmail, fieldName},
		{"an empty email", goodName, "", fieldEmail},
		{"an email that is not one", goodName, "not-an-email", fieldEmail},
		{"an email with a display name", goodName, displayNameEmail, fieldEmail},
		{"an email in angle brackets", goodName, bracketedEmail, fieldEmail},
		{"an email with a quoted display name", goodName, `"Bot" <bot@example.org>`, fieldEmail},
		{"an email with a space before it", goodName, " bot@example.org", fieldEmail},
		{"an email with a space after it", goodName, "bot@example.org ", fieldEmail},
		{"an email with a newline after it", goodName, "bot@example.org\n", fieldEmail},
		{"a list of two addresses", goodName, "a@example.org, b@example.org", fieldEmail},
		{"an email with two at signs", goodName, "bot@@example.org", fieldEmail},
		{"an email with no domain", goodName, "bot@", fieldEmail},
		{"an email with a comment", goodName, "bot@example.org (the bot)", fieldEmail},
	}
	for _, tt := range tests {
		t.Run(tt.what, func(t *testing.T) {
			err := ValidateAuthor(tt.name, tt.email)
			if err == nil {
				t.Fatalf("ValidateAuthor(%q, %q) = nil, want an error", tt.name, tt.email)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to name the %s", err, tt.want)
			}
		})
	}
}

// Git takes a space, a control character and one of . , : ; " ' \ from the start and the end of a name and of an email
// when it writes the author, so a value that starts or ends with one would be written as another.
func TestValidateAuthorRefusesAValueThatGitWouldTrim(t *testing.T) {
	tests := []struct{ what, name, email, want string }{
		{"a name with a space before it", " bot", goodEmail, fieldName},
		{"a name with a space after it", "bot ", goodEmail, fieldName},
		{"a name with a period after it", "Treebind Bot.", goodEmail, fieldName},
		{"a name with a period before it", ".bot", goodEmail, fieldName},
		{"a name with a comma after it", "bot,", goodEmail, fieldName},
		{"a name with a colon before it", ":bot", goodEmail, fieldName},
		{"a name with a semicolon after it", "bot;", goodEmail, fieldName},
		{"a name in double quotes", `"bot"`, goodEmail, fieldName},
		{"a name in single quotes", "'bot'", goodEmail, fieldName},
		{"a name with a backslash before it", `\bot`, goodEmail, fieldName},
		{"an email with a quote before it", goodName, "'bot@example.org", fieldEmail},
		{"an email with a quote after it", goodName, "bot@example.org'", fieldEmail},
	}
	for _, tt := range tests {
		t.Run(tt.what, func(t *testing.T) {
			err := ValidateAuthor(tt.name, tt.email)
			if err == nil {
				t.Fatalf("ValidateAuthor(%q, %q) = nil, want an error", tt.name, tt.email)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "start or end") {
				t.Errorf("error = %q, want it to name the %s and to say that it must not start or end so", err, tt.want)
			}
		})
	}
}

// The same characters are fine inside a value, where Git leaves them, and so are the forms of an email that Git keeps.
func TestValidateAuthorAcceptsWhatGitKeeps(t *testing.T) {
	tests := []struct{ name, email string }{
		{"bo.t", goodEmail},
		{"Treebind Bot, the sync agent", goodEmail},
		{"a:b;c", goodEmail},
		{"it's the bot", goodEmail},
		{"bøt", "bøt@example.org"},
		{goodName, "o'brien@example.org"},
		{goodName, "bot@[127.0.0.1]"},
		// Letters whose last byte is that of a period, a space or a quote: only a byte that is a character of its own
		// is taken from the end by Git, and not the end of a longer one.
		{"bot\u012e", goodEmail},
		{"bot\u0127", goodEmail},
		{"Treebind\u2019", goodEmail},
	}
	for _, tt := range tests {
		if err := ValidateAuthor(tt.name, tt.email); err != nil {
			t.Errorf("ValidateAuthor(%q, %q) = %v, want nil", tt.name, tt.email, err)
		}
	}
}

func TestRenderRefusesAMessageThatIsTooLongAndStopsAtOnce(t *testing.T) {
	// A loop that would build a gigabyte is stopped when the message passes the limit, and does not run to its end.
	for _, tt := range []struct {
		name string
		tmpl string
		data Data
	}{
		{"a range over a number in the template", "{{range 1000000000}}x{{end}}", Data{}},
		{"a range over the count", "{{range .Count}}x{{end}}", Data{Count: 1 << 30}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.tmpl, tt.data)
			if err == nil || got != "" || !strings.Contains(err.Error(), "more than") {
				t.Errorf("Render = %q, %v, want an error that says that the message is longer than the limit", got, err)
			}
		})
	}
}

func TestRenderAcceptsAMessageOfExactlyTheLimit(t *testing.T) {
	at, err := Render("{{range .Count}}x{{end}}", Data{Count: MaxMessageBytes})
	if err != nil || len(at) != MaxMessageBytes {
		t.Fatalf("a message of %d bytes: got %d bytes, %v, want it accepted", MaxMessageBytes, len(at), err)
	}
	if _, err := Render("{{range .Count}}x{{end}}", Data{Count: MaxMessageBytes + 1}); err == nil {
		t.Errorf("a message of %d bytes was accepted, want the limit to be %d", MaxMessageBytes+1, MaxMessageBytes)
	}
}

// The limit is about 64 KiB: a message of a few dozen kilobytes is one, and one of a hundred is not. The tests above hold the
// edge of whatever the constant is, and this holds the size of it.
func TestTheLimitIsOfTheOrderOfSixtyFourKilobytes(t *testing.T) {
	if _, err := Render(strings.Repeat("x", 60_000), Data{}); err != nil {
		t.Errorf("a message of 60,000 bytes: %v, want it accepted", err)
	}
	if _, err := Render(strings.Repeat("x", 70_000), Data{}); err == nil {
		t.Error("a message of 70,000 bytes was accepted, want the limit to be 64 KiB")
	}
}

func TestRenderRefusesOneWriteThatIsOverTheLimitOnItsOwn(t *testing.T) {
	// A template that is one long text writes it in one piece, which is more than the limit before anything is collected.
	if _, err := Render(strings.Repeat("x", MaxMessageBytes+1), Data{}); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("error = %v, want it to say that the message is longer than the limit", err)
	}
	if got, err := Render(strings.Repeat("x", MaxMessageBytes), Data{}); err != nil || len(got) != MaxMessageBytes {
		t.Errorf("a template of exactly the limit: %d bytes, %v, want it accepted", len(got), err)
	}
}

func TestPrintfCannotBuildAMessageOfGigabytes(t *testing.T) {
	// text/template builds a printf in memory before it writes it, so the limit above cannot stop it: fmt does, by
	// refusing a width over a million. What it makes is a few bytes of an error, which is short.
	got, err := Render(`{{printf "%1000000000d" 1}}`, Data{})
	if err != nil && !strings.Contains(err.Error(), "commit message") {
		t.Fatalf("error = %v", err)
	}
	if len(got) > 1<<20 {
		t.Errorf("a printf made a message of %d bytes", len(got))
	}
}

func TestValidateAuthorSaysThatAnEmailWithMoreThanTheAddressIsNotABareOne(t *testing.T) {
	for _, email := range []string{displayNameEmail, bracketedEmail, `"Bot" <bot@example.org>`, "bot@example.org (the bot)"} {
		if err := ValidateAuthor(goodName, email); err == nil || !strings.Contains(err.Error(), "one bare address") {
			t.Errorf("ValidateAuthor(%q, %q) = %v, want it to say that the email must be one bare address", goodName, email, err)
		}
	}
}

func TestValidateAuthorSaysThatAnEmptyValueIsEmpty(t *testing.T) {
	for _, tt := range []struct{ name, email, want string }{
		{"", goodEmail, "author name is empty"},
		{"   ", goodEmail, "author name is empty"},
		{shortName, "", "author email is empty"},
	} {
		if err := ValidateAuthor(tt.name, tt.email); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ValidateAuthor(%q, %q) = %v, want it to say %q", tt.name, tt.email, err, tt.want)
		}
	}
}

func TestValidateAuthorNamesTheNameWhenBothAreWrong(t *testing.T) {
	err := ValidateAuthor("", "not-an-email")
	if err == nil || !strings.Contains(err.Error(), fieldName) || !strings.Contains(err.Error(), fieldEmail) {
		t.Errorf("error = %v, want it to name both: one wrong value does not hide the other", err)
	}
}
