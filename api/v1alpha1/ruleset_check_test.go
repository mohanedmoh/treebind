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

package v1alpha1

import "testing"

// TestRulesetCheck fails on purpose. It exists only on a throwaway pull request
// that checks the ci-gate rule blocks a merge. Do not merge it.
func TestRulesetCheck(t *testing.T) {
	t.Fatal("intentional failure: checking that ci-gate blocks the merge")
}
