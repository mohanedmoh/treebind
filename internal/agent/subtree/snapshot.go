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
	"regexp"
	"strconv"
	"strings"
)

// The trailers of a snapshot. The source is the commit of the repository that the snapshot is the directory of, and the path is
// the directory. They are in the commit and not in a file of the state directory, so that what a checkout was made for cannot
// drift from the commit it is at, and a checkout of the whole repository, which has neither, is told from one of a directory.
const (
	trailerSource = "Treebind-Source"
	trailerPath   = "Treebind-Path"
)

// snapshot is what a snapshot says of itself.
type snapshot struct {
	// source is the commit of the repository, and path the directory of it, that the snapshot is the files of.
	source, path string
}

// snapshotMessage is the message of the snapshot of the directory at a commit of the repository. The path is quoted: the CRD
// takes a name with a newline in it, which would end the trailer, and it is not in the subject for the same reason.
func snapshotMessage(source, dir string) string {
	return "treebind: the files of a directory of the repository at " + short(source) + "\n\n" +
		trailerSource + ": " + source + "\n" + trailerPath + ": " + strconv.Quote(dir) + "\n"
}

// objectName is the name of a commit as Git writes it, with SHA-1.
var objectName = regexp.MustCompile(`^[0-9a-f]{40}$`)

// parseSnapshot reads what a message says of a snapshot. A message is a snapshot's only when its last paragraph, after the
// subject, is the two trailers and nothing else, the source is the name of a commit and the path is a quoted string that is not
// empty. The commits that the agent makes for the application's changes have the message of a template that the Treebind sets,
// and what the template says must not make one a snapshot by accident.
func parseSnapshot(message string) (snapshot, bool) {
	message = strings.TrimRight(message, "\n")
	cut := strings.LastIndex(message, "\n\n")
	if cut < 0 {
		return snapshot{}, false
	}
	lines := strings.Split(message[cut+2:], "\n")
	if len(lines) != 2 {
		return snapshot{}, false
	}
	source, ok := strings.CutPrefix(lines[0], trailerSource+": ")
	if !ok || !objectName.MatchString(source) {
		return snapshot{}, false
	}
	quoted, ok := strings.CutPrefix(lines[1], trailerPath+": ")
	if !ok {
		return snapshot{}, false
	}
	dir, err := strconv.Unquote(quoted)
	if err != nil || dir == "" {
		return snapshot{}, false
	}
	return snapshot{source: source, path: dir}, true
}

// short is the start of the name of a commit, for a message.
func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
