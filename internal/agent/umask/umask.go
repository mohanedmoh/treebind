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

// Package umask sets the file creation mask of the sync agent.
package umask

import "syscall"

// Group is the agent's umask. It leaves files at 0664 and directories at 0775,
// so the application that shares the volume, running as another user in the same
// group, can still change what the agent wrote (R-12).
const Group = 0o002

// Set applies Group to the process and returns the mask that was in effect.
// The mask is inherited by the Git commands the agent starts, so call it before
// anything is written or started.
func Set() (previous int) {
	return syscall.Umask(Group)
}
