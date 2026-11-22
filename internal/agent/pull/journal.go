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

package pull

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// journalName is the file in the state directory that says the agent began to apply an update, and has
// not said that it is done. It is written before the merge and removed after it, so that a start that
// finds it knows that the files the update changes may be half written (docs/adr/0028).
const journalName = "updating"

// UpdateCutOff says whether the journal of an update is in the state directory of the layout: a pull began to apply an
// update and did not end it, so that the files that the update changes may be half written, and are the agent's to put
// back and not the application's to commit. Only the repair at the start of the next pull puts them right and removes
// the journal (docs/adr/0028), so the commit step commits nothing while it is there, and the first pull of a start that
// finds it does not wait for its phase. It looks with one stat and reads nothing: a journal that cannot be used is
// there all the same, and the pull removes it. An error is a state directory that could not be looked at, and the
// answer is then that the update may be cut off, so that what cannot be told is not taken for the application's.
func UpdateCutOff(l layout.Layout) (bool, error) {
	_, err := os.Lstat(filepath.Join(l.StateDir, journalName))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return true, err
}

// journalVersion is the format of the journal. A journal of another format is not read.
const journalVersion = 1

// maxJournal is the most that a journal can be: two commit ids and the words around them.
const maxJournal = 1 << 10

// commitID is what a journal holds: the id of a commit, SHA-1 or SHA-256.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// journal is the update that was begun: the commit the checkout was at, which is empty when it had none,
// and the commit it was going to.
type journal struct {
	Version int    `json:"version"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// writeJournal records the update that is about to be applied. It is written to a file of its own and
// renamed into place, so that it is there whole or not at all.
func (p *Puller) writeJournal(from, to string) error {
	data, err := json.Marshal(journal{Version: journalVersion, From: from, To: to})
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(p.o.Layout.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	const temporary = journalName + ".new"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		// The journal says what a start that is not the end of this process has to look for.
		err = file.Sync()
	}
	if closed := file.Close(); err == nil {
		err = closed
	}
	if err != nil {
		_ = root.Remove(temporary)
		return err
	}
	return root.Rename(temporary, journalName)
}

// readJournal looks for the journal with one stat, and reads it when it is there. present is false when
// there is none. A journal that is there and cannot be used, because it cannot be read, is torn, or is not
// one of this agent's, is present with the problem: it is no journal, and says so.
func (p *Puller) readJournal() (j journal, present bool, problem error) {
	root, err := os.OpenRoot(p.o.Layout.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return journal{}, false, nil
	}
	if err != nil {
		return journal{}, true, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(journalName)
	if errors.Is(err, fs.ErrNotExist) {
		return journal{}, false, nil
	}
	if err != nil {
		return journal{}, true, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxJournal {
		return journal{}, true, errors.New("it is not a file of the size of a journal")
	}
	data, err := root.ReadFile(journalName)
	if err != nil {
		return journal{}, true, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&j); err != nil {
		return journal{}, true, fmt.Errorf("it is torn: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return journal{}, true, errors.New("it is torn: there is more after it")
	}
	switch {
	case j.Version != journalVersion:
		return journal{}, true, fmt.Errorf("it is of the version %d, which this agent does not read", j.Version)
	case !commitID.MatchString(j.To) || (j.From != "" && !commitID.MatchString(j.From)):
		return journal{}, true, errors.New("it names a commit that is not the id of one")
	case j.From == j.To:
		return journal{}, true, errors.New("it names one commit for both ends of the update")
	}
	return j, true, nil
}

// removeJournal removes the journal. One that is not there is as good as removed.
func (p *Puller) removeJournal() error {
	root, err := os.OpenRoot(p.o.Layout.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(journalName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
