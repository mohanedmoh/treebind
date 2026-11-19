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

// Package loops has no code. Its tests are the ways in which what the agent writes could come back to it as something
// new, an echo loop (R-18, docs/adr/0035), each made to happen on real repositories with the real pull, scan, commit and
// push packages: what a pull writes is not a change of the application, the application's write right after it still is,
// the steps of the agent do not wake its own watcher, a commit that was pushed is up to date at the next pull, and two
// Treebinds that edit and resolve at random end at one tree. Where the agent has a guard against a loop, the test of the
// guard is in the package that has it.
package loops
