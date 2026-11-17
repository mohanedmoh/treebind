//go:build linux

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

package scan

import (
	"os"
	"syscall"
	"testing"
)

func TestWhatTheAgentSawIsWhatTheFileSystemSays(t *testing.T) {
	w := newWorld(t, "", base)
	w.write("f.txt", "12345")
	info, err := os.Lstat(w.path("f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("the file system does not give the numbers of a file")
	}
	want := Seen{
		Present: true, Mode: info.Mode(), Size: 5, ModTime: info.ModTime().UnixNano(),
		ChangeTime: stat.Ctim.Nano(), Inode: stat.Ino,
	}
	if got := changes(t, w.scan())["f.txt"].Seen; got != want {
		t.Errorf("seen = %+v, want %+v", got, want)
	}
}
