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

import "strings"

const (
	// RealPrefix is where the real branch of the repository is kept: the tip that the snapshot at the tracking ref is the directory
	// of (docs/adr/0036). It is moved when the snapshot is in place and never before, so that it is never ahead of the snapshot,
	// because the pull fetches only when the tip the repository has is not the one that is held.
	RealPrefix = "refs/treebind/real/"
	// FetchPrefix is where a fetch brings the tip of the branch, to be made a snapshot of and then held at RealPrefix.
	FetchPrefix = "refs/treebind/fetched/"
	// trackingPrefix is where the snapshots of the branch are kept. It is the ref that a checkout of the whole repository keeps
	// the repository's own tip in, so that the pull and the push read the same ref in both cases.
	trackingPrefix = "refs/remotes/origin/"
	headsPrefix    = "refs/heads/"
)

// RealRef is the ref that holds the tip of the branch of the repository that the snapshot of the branch is the directory of.
func RealRef(branch string) string { return RealPrefix + branch }

// FetchRef is the ref that a fetch of the branch brings the tip to. Project makes the snapshot of the directory at it and then moves
// RealRef to it.
func FetchRef(branch string) string { return FetchPrefix + branch }

// IsOwnRef reports whether a ref is one that the snapshots keep, and so one that the agent made.
func IsOwnRef(ref string) bool {
	return strings.HasPrefix(ref, RealPrefix) || strings.HasPrefix(ref, FetchPrefix)
}

// TrackingRef is the ref that holds the snapshot at the tip of the branch.
func TrackingRef(branch string) string { return trackingPrefix + branch }

// emptyTree is the tree that has no entry. The agent's repositories use SHA-1.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
