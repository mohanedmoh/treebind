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
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// The modes that a tree gives its entries, as Git writes them in the tree object: a directory has no leading zero.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeLink       = "120000"
	modeDirectory  = "40000"
)

// idLength is the length of an object name in the tree object, in bytes. The agent's repositories use SHA-1.
const idLength = 20

// entry is one line of a tree object.
type entry struct {
	mode, name string
	id         []byte
}

// sortKey is what Git compares the entries of a tree by: the name, and for a directory the name with a slash at the end.
func (e entry) sortKey() string {
	if e.mode == modeDirectory {
		return e.name + "/"
	}
	return e.name
}

// parseTree reads a tree object, as `git cat-file tree` writes it: for each entry a mode, a space, a name, a NUL and the
// object name as bytes. The entries are not checked to be in order, which Git does when it reads the tree.
func parseTree(raw []byte) ([]entry, error) {
	var entries []entry
	for len(raw) > 0 {
		space := bytes.IndexByte(raw, ' ')
		if space < 1 {
			return nil, errors.New("an entry of a tree has no mode")
		}
		if _, err := strconv.ParseUint(string(raw[:space]), 8, 32); err != nil {
			return nil, fmt.Errorf("an entry of a tree has the mode %q, which is not a number", raw[:space])
		}
		nul := bytes.IndexByte(raw[space+1:], 0)
		if nul < 1 {
			return nil, errors.New("an entry of a tree has no name")
		}
		nul += space + 1
		rest := raw[nul+1:]
		if len(rest) < idLength {
			return nil, errors.New("a tree is cut in the middle of an entry")
		}
		entries = append(entries, entry{mode: string(raw[:space]), name: string(raw[space+1 : nul]), id: slices.Clone(rest[:idLength])})
		raw = rest[idLength:]
	}
	return entries, nil
}

// formatTree writes the entries as a tree object, in the order that Git keeps them in.
func formatTree(entries []entry) []byte {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b entry) int { return bytes.Compare([]byte(a.sortKey()), []byte(b.sortKey())) })
	var out bytes.Buffer
	for _, e := range sorted {
		out.WriteString(e.mode + " " + e.name)
		out.WriteByte(0)
		out.Write(e.id)
	}
	return out.Bytes()
}

// objects reads and writes the trees of one repository through the Runner, which has no standard input: a tree that is written
// is given to Git as a file, in the state directory, where the scan leaves it out.
type objects struct {
	git *git.Runner
	// scratch is the directory the file is made in.
	scratch string
}

// treeFile is the file that a tree is written to for Git to read.
const treeFile = "subtree-tree"

// tree reads the entries of a tree.
func (o objects) tree(ctx context.Context, id string) ([]entry, error) {
	out, err := o.git.Run(ctx, "cat-file", "tree", id)
	if err != nil {
		return nil, err
	}
	return parseTree(out.Stdout)
}

// write makes a tree of the entries and returns its name. The empty tree is not written: Git knows it.
func (o objects) write(ctx context.Context, entries []entry) (string, error) {
	if len(entries) == 0 {
		return emptyTree, nil
	}
	// The state directory can be written by an application that runs as the agent's own user, so the file is made through a root that
	// does not follow a link out of it, and exclusively, after what a run that was killed left was removed: a link that was
	// planted where the file goes is removed and not followed, and what it points to is not written over.
	root, err := os.OpenRoot(o.scratch)
	if err != nil {
		return "", fmt.Errorf("the tree cannot be written in %s: %w", o.scratch, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(treeFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the file for the tree cannot be replaced in %s: %w", o.scratch, err)
	}
	if err := writeExclusively(root, treeFile, formatTree(entries)); err != nil {
		return "", fmt.Errorf("the tree cannot be written to %s: %w", filepath.Join(o.scratch, treeFile), err)
	}
	defer func() { _ = root.Remove(treeFile) }()
	file := filepath.Join(o.scratch, treeFile)
	out, err := o.git.Run(ctx, "hash-object", "-w", "-t", "tree", "--", file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}

// writeExclusively makes the file in the root, and fails when there is one.
func writeExclusively(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// replace returns the tree of root with the directory that the segments name replaced by the tree sub. A directory that is not
// there is made, and one that sub empties is removed with every parent that it leaves empty, as Git does not keep a directory
// with nothing in it. root may be the empty tree. The only entries that change are the ones on the way to the directory: what
// is beside them is copied as Git has it, with its mode and its name.
func (o objects) replace(ctx context.Context, root string, segments []string, sub string) (string, error) {
	if len(segments) == 0 {
		return sub, nil
	}
	var entries []entry
	if root != emptyTree {
		var err error
		if entries, err = o.tree(ctx, root); err != nil {
			return "", err
		}
	}
	var child string
	kept := entries[:0:0]
	for _, e := range entries {
		switch {
		case e.name != segments[0]:
			kept = append(kept, e)
		case e.mode != modeDirectory:
			return "", fmt.Errorf("%q is a file, not a directory", segments[0])
		default:
			child = hex.EncodeToString(e.id)
		}
	}
	if child == "" {
		child = emptyTree
	}
	replaced, err := o.replace(ctx, child, segments[1:], sub)
	if err != nil {
		return "", err
	}
	if replaced != emptyTree {
		id, err := hex.DecodeString(replaced)
		if err != nil {
			return "", fmt.Errorf("%q is not the name of an object: %w", replaced, err)
		}
		kept = append(kept, entry{mode: modeDirectory, name: segments[0], id: id})
	}
	return o.write(ctx, kept)
}
