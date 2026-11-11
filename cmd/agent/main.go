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

// Command agent is the Treebind sync agent.
//
// The agent is not implemented yet. This entry point exists so that the build
// and the release pipeline already produce both images; the sync loop replaces
// it in milestone M1.
package main

import (
	"fmt"
	"os"

	"github.com/mohanedmoh/treebind/internal/agent/umask"
)

func main() {
	umask.Set()
	fmt.Fprintln(os.Stderr, "treebind-agent: the sync agent is not implemented yet")
	os.Exit(1)
}
