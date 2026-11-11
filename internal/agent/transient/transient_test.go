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
	"testing"
	"time"
)

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"a.tmp", true},
		{"dir/a.swp", true},
		{"a.swo", true},
		{"a.swx", true},
		{"a.txt~", true},
		{".#a.txt", true},
		{"#a.txt#", true},
		{".~lock.a.odt#", true},
		{"a.txt", false},
		{"a.tmpl", false},
		{"tmp", false},
		{"swap.go", false},
		{"a~b.txt", false},
		{"#a.txt", false},
		{".hidden", false},
		{"dir.tmp/a.txt", false},
		{"A.TMP", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.name); got != tc.want {
				t.Fatalf("IsTransient(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestDefaultTransientPatternsAreValid(t *testing.T) {
	for _, pattern := range defaultTransientPatterns {
		if _, err := path.Match(pattern, "name"); err != nil {
			t.Fatalf("invalid default pattern %q: %v", pattern, err)
		}
	}
}

func TestSettled(t *testing.T) {
	lastChange := time.Unix(95, 0)
	settle := 5 * time.Second

	cases := []struct {
		name       string
		lastChange time.Time
		now        time.Time
		settle     time.Duration
		want       bool
	}{
		{"exactly settle", lastChange, lastChange.Add(settle), settle, true},
		{"one nanosecond less", lastChange, lastChange.Add(settle - time.Nanosecond), settle, false},
		{"clock stepped back", lastChange, lastChange.Add(-time.Second), settle, false},
		{"zero settle equal", lastChange, lastChange, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Settled(tc.lastChange, tc.now, tc.settle); got != tc.want {
				t.Fatalf("Settled(%v, %v, %v) = %v, want %v", tc.lastChange, tc.now, tc.settle, got, tc.want)
			}
		})
	}
}
