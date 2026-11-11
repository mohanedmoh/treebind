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

package transient

import (
	"path"
	"time"
)

// Default transient file patterns. Matching is applied to the final element of a
// slash-separated path only, and is case-sensitive.
var defaultTransientPatterns = []string{
	"*.tmp",
	"*.swp",
	"*.swo",
	"*.swx",
	"*~",
	".#*",
	"#*#",
	".~lock.*#",
}

// DefaultSettle is the quiet period the sync agent waits before committing a
// changed file.
const DefaultSettle = 5 * time.Second

// IsTransient reports whether the final element of name matches one of the
// default transient file patterns.
func IsTransient(name string) bool {
	base := path.Base(name)
	for _, pattern := range defaultTransientPatterns {
		matched, err := path.Match(pattern, base)
		if err == nil && matched {
			return true
		}
	}
	return false
}

// Settled reports whether at least settle has passed since lastChange.
func Settled(lastChange, now time.Time, settle time.Duration) bool {
	return now.Sub(lastChange) >= settle
}
