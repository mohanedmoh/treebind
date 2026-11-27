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

// Package passwd writes the passwd file that the sync container sees at /etc/passwd, so that the user it runs as has
// an entry whatever user ID the platform gives the pod (docs/adr/0042). OpenSSH looks its own user up in the passwd
// file and refuses to run without an entry ("No user exists for uid"), and the image only has one for its own user,
// 65532. The root filesystem is read-only, so the file is written into a volume of the pod, by a container that runs
// as the same user before the sync container starts, and mounted over /etc/passwd.
package passwd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// SystemFile is the passwd file of the image, which the written file starts from.
const SystemFile = "/etc/passwd"

// Command is the argument that makes the agent write the passwd file, to the path that follows it, and exit. The
// operator starts the agent image with it in an init container of the sync pod.
const Command = "write-passwd"

// The entry that is added for a user ID that the image does not know. Only the user ID matters to ssh, which looks
// the user up by it, and to Git, which never asks: the agent names the author and the committer of its commits. Git
// and ssh read nothing from a home directory (HOME=/nonexistent in the Runner, and ssh -F /dev/null), so the entry
// names none that could be there, and no shell.
const (
	userName = "treebind"
	homeDir  = "/nonexistent"
	shell    = "/sbin/nologin"
)

// fileMode is the mode of the written file. Every program of the container reads it, and nothing writes it again.
const fileMode fs.FileMode = 0o444

// WithUser returns the passwd file with an entry for uid: the file as it is when one of its entries has that user ID,
// and the file with an entry of the agent's added at its end, with the group gid, when none has.
func WithUser(file []byte, uid, gid int) []byte {
	if hasUser(file, uid) {
		return file
	}
	out := bytes.Clone(file)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return fmt.Appendf(out, "%s:x:%d:%d:%s:%s:%s\n", userName, uid, gid, userName, homeDir, shell)
}

// hasUser says whether an entry of the file has the user ID in its third field.
func hasUser(file []byte, uid int) bool {
	want := []byte(strconv.Itoa(uid))
	for line := range bytes.Lines(file) {
		fields := bytes.Split(bytes.TrimRight(line, "\n"), []byte(":"))
		if len(fields) >= 3 && bytes.Equal(fields[2], want) {
			return true
		}
	}
	return false
}

// Write writes WithUser of the file at source, for the user and the group that the process runs as, to target. A
// source that is not there counts as an empty file. The file is written beside the target and renamed over it, so
// that a target from an earlier start of the pod is replaced whole or not at all.
func Write(source, target string) error {
	file, err := os.ReadFile(source)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the passwd file of the image: %w", err)
	}
	out := WithUser(file, os.Getuid(), os.Getgid())

	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+"-*")
	if err != nil {
		return fmt.Errorf("writing the passwd file: %w", err)
	}
	_, writeErr := temp.Write(out)
	err = errors.Join(writeErr, temp.Chmod(fileMode), temp.Close())
	if err == nil {
		err = os.Rename(temp.Name(), target)
	}
	if err != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("writing the passwd file: %w", err)
	}
	return nil
}
