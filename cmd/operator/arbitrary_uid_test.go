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

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The setting is off unless it is said, and an install.yaml or a chart of an older release sets nothing. A value that
// is not a boolean stops the operator, so that a typo does not leave the sync pods without what was asked for.
func TestTheSettingForAnyUserIDIsOffUnlessItIsTrue(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "false": false, "0": false, "true": true, "1": true, "TRUE": true,
	} {
		got, err := parseArbitraryUserID(value)
		if err != nil || got != want {
			t.Errorf("%q: %v, %v, want %v and no error", value, got, err, want)
		}
	}
	for _, value := range []string{"yes", "on", "<nil>", " true"} {
		if _, err := parseArbitraryUserID(value); err == nil || !strings.Contains(err.Error(), arbitraryUserIDEnv) {
			t.Errorf("%q: error %v, want one that names %s", value, err, arbitraryUserIDEnv)
		}
	}
}

// main cannot be run in a test, so this holds the wiring as it reads: the flag defaults to the variable, a bad value
// stops the operator, and both controllers get the setting, because the poller computes the hash of the template
// that the reconcile of the spec makes.
func TestTheSettingForAnyUserIDReachesBothControllers(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, want := range []string{
		`flag.StringVar(&arbitraryUserID, "agent-arbitrary-user-id", os.Getenv(arbitraryUserIDEnv)`,
		`parseArbitraryUserID(arbitraryUserID)`,
		`Invalid --agent-arbitrary-user-id`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("main.go does not hold %q", want)
		}
	}
	// gofmt aligns the fields of a struct, so the space after the colon is not fixed.
	field := regexp.MustCompile(`AgentArbitraryUserID:\s+agentArbitraryUserID,`)
	if got := len(field.FindAllString(text, -1)); got != 2 {
		t.Errorf("main.go gives the setting to %d controllers, want 2", got)
	}
}
