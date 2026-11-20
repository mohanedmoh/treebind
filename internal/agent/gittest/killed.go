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

package gittest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// KilledAfter is a stand-in for git, for the Git option of a Runner, that runs what it is asked, and after the nth command that
// has the subcommand among its arguments has run is the end of what Git can do: every command from then on fails without running,
// so that nothing the agent would do to clean up after itself hides what is left. It is what an agent that is killed between two
// writes leaves. It uses what a shell has built in and Git, because the tests run in the image of the agent too, which has no
// cat, rm or mktemp.
func KilledAfter(t *testing.T, subcommand string, n int) string {
	t.Helper()
	dir := TempDir(t)
	binary := filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
D='%s'
if [ -e "$D/dead" ]; then echo 'the agent was killed' >&2; exit 137; fi
hit=
for arg in "$@"; do
  if [ "$arg" = '%s' ]; then hit=yes; fi
done
'%s' "$@"
rc=$?
if [ -n "$hit" ]; then
  : > "$D/hit.$$"
  count=0
  for f in "$D"/hit.*; do [ -e "$f" ] && count=$((count+1)); done
  if [ "$count" -ge %d ]; then : > "$D/dead"; fi
fi
exit $rc
`, dir, subcommand, Binary(t), n)
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}
