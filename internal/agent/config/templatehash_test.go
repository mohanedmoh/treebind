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

package config

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// A SHA-256 as the operator writes it: 64 lowercase hexadecimal digits.
const templateHash = "27ed59bdbee75da599c2e1f63aa5ca8a94c718e28c03fe67345826eb4ff8bab0"

func TestFromEnvTakesTheHashOfThePodTemplate(t *testing.T) {
	env := complete()
	env[EnvTemplateHash] = templateHash

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.TemplateHash != templateHash {
		t.Errorf("TemplateHash = %q, want %q", config.TemplateHash, templateHash)
	}
}

// A pod from before the operator set the variable has none, and runs as it did.
func TestFromEnvLeavesTheHashOfThePodTemplateEmptyWhenItIsNotSet(t *testing.T) {
	for name, value := range map[string]string{"not set": "", "only a newline": "\n"} {
		t.Run(name, func(t *testing.T) {
			env := complete()
			env[EnvTemplateHash] = value

			config, err := fromMap(env)

			if err != nil || config.TemplateHash != "" {
				t.Errorf("TemplateHash = %q, %v, want none and no problem", config.TemplateHash, err)
			}
		})
	}
}

func TestFromEnvTrimsTheHashOfThePodTemplate(t *testing.T) {
	env := complete()
	env[EnvTemplateHash] = " " + templateHash + "\n"

	config, err := fromMap(env)

	if err != nil || config.TemplateHash != templateHash {
		t.Errorf("TemplateHash = %q, %v, want %q", config.TemplateHash, err, templateHash)
	}
}

func TestFromEnvSaysABadHashOfThePodTemplateWithTheOtherProblems(t *testing.T) {
	env := complete()
	env[EnvTemplateHash] = "not-a-hash"
	env[EnvNamespace] = ""

	_, err := fromMap(env)

	if err == nil || !strings.Contains(err.Error(), EnvTemplateHash+" must be 64 lowercase hexadecimal digits") ||
		!strings.Contains(err.Error(), EnvNamespace+" is not set") {
		t.Errorf("error = %v, want both problems at once", err)
	}
}

func TestCheckTemplateHash(t *testing.T) {
	tests := []struct {
		name  string
		value string
		ok    bool
	}{
		{"none says the pod was started without one", "", true},
		{"64 lowercase hexadecimal digits", templateHash, true},
		{"all digits", strings.Repeat("0", 64), true},
		{"all letters", strings.Repeat("f", 64), true},
		{"63 digits", templateHash[1:], false},
		{"65 digits", templateHash + "0", false},
		{"a SHA-1", templateHash[:40], false},
		{"uppercase, which the operator never writes", strings.ToUpper(templateHash), false},
		{"one uppercase digit", templateHash[:63] + "A", false},
		{"a letter past f", templateHash[:63] + "g", false},
		{"a prefix of the algorithm", "sha256:" + templateHash[:57], false},
		{"64 characters that are not digits", strings.Repeat("-", 64), false},
		{"a space inside", templateHash[:32] + " " + templateHash[33:], false},
		{"a newline at the end, which FromEnv trims and a caller does not", templateHash[:63] + "\n", false},
		{"64 bytes of two-byte characters", strings.Repeat("é", 32), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckTemplateHash(tt.value)

			if (err == nil) != tt.ok {
				t.Errorf("CheckTemplateHash(%q) = %v, want ok = %v", tt.value, err, tt.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "must be 64 lowercase hexadecimal digits") {
				t.Errorf("CheckTemplateHash(%q) = %v, want a problem that says what a hash is", tt.value, err)
			}
		})
	}
}

// The operator hashes the pod template with SHA-256 and writes the digest in hexadecimal.
func TestTheLengthOfTheHashOfThePodTemplateIsThatOfASHA256InHexadecimal(t *testing.T) {
	if TemplateHashLength != 2*sha256.Size {
		t.Errorf("TemplateHashLength = %d, want %d", TemplateHashLength, 2*sha256.Size)
	}
}
