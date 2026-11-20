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

package subtree

import (
	"bytes"
	"strings"
	"testing"
)

func id(b byte) []byte { return bytes.Repeat([]byte{b}, 20) }

// A tree that is parsed and written again is the same bytes, whatever the names hold.
func TestATreeIsTheSameBytesAfterItIsParsedAndWrittenAgain(t *testing.T) {
	entries := []entry{
		{mode: modeFile, name: "a b.json", id: id(1)},
		{mode: modeFile, name: "é.json", id: id(2)},
		{mode: modeFile, name: "new\nline", id: id(3)},
		{mode: modeExecutable, name: "run.sh", id: id(4)},
		{mode: modeLink, name: "link", id: id(5)},
		{mode: modeDirectory, name: "sub", id: id(6)},
	}
	raw := formatTree(entries)
	parsed, err := parseTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if again := formatTree(parsed); !bytes.Equal(again, raw) {
		t.Errorf("the tree changed:\n%q\n%q", raw, again)
	}
	if len(parsed) != len(entries) {
		t.Errorf("parsed %d entries, want %d", len(parsed), len(entries))
	}
}

// Git sorts the entries of a tree by name, a directory as if its name ended with a slash: "a-b" and "a.b" come before the
// directory "a", and a tree that is out of order is a tree that Git refuses.
func TestEntriesAreInTheOrderOfGitWhereADirectoryEndsWithASlash(t *testing.T) {
	raw := formatTree([]entry{
		{mode: modeDirectory, name: "a", id: id(1)},
		{mode: modeFile, name: "a.b", id: id(2)},
		{mode: modeFile, name: "a-b", id: id(3)},
		{mode: modeFile, name: "a0", id: id(4)},
	})
	parsed, err := parseTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(parsed))
	for _, e := range parsed {
		names = append(names, e.name)
	}
	if got, want := strings.Join(names, ","), "a-b,a.b,a,a0"; got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
}

func TestATreeThatIsCutOrMalformedIsRefused(t *testing.T) {
	whole := formatTree([]entry{{mode: modeFile, name: "x", id: id(1)}})
	tests := map[string][]byte{
		"cut in the id":            whole[:len(whole)-1],
		"no name":                  []byte("100644 "),
		"no space":                 []byte("100644x\x00" + strings.Repeat("a", 20)),
		"an empty name":            append([]byte("100644 \x00"), id(1)...),
		"a mode that is no number": append([]byte("10x644 x\x00"), id(1)...),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTree(raw); err == nil {
				t.Error("want an error")
			}
		})
	}
}
