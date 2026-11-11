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

package umask

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestSetKeepsNewFilesGroupWritable starts from the strictest common mask, so the
// modes below can only come from Set.
func TestSetKeepsNewFilesGroupWritable(t *testing.T) {
	restore := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(restore) })

	if previous := Set(); previous != 0o077 {
		t.Errorf("Set returned the previous mask %#o, want 0o077", previous)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "dir")
	if err := os.Mkdir(sub, 0o777); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{file: 0o664, sub: 0o775} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %#o, want %#o", filepath.Base(path), got, want)
		}
	}
}
