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

// Package config reads the agent's settings from its environment. The operator
// puts them in the sync pod (docs/adr/0011). Nothing in them is secret: the agent
// holds no credential, so every value is a name, a path or a public fact about the
// cluster.
package config

import (
	"errors"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The names of the environment variables.
const (
	EnvName      = "TREEBIND_NAME"
	EnvNamespace = "TREEBIND_NAMESPACE"
	EnvMountPath = "TREEBIND_MOUNT_PATH"
	// EnvSubPath is the directory of the claim that is synced, relative to the mount
	// path. It is not set when the whole claim is (docs/adr/0016).
	EnvSubPath = "TREEBIND_SUB_PATH"

	EnvStatusAddr     = "TREEBIND_STATUS_ADDR"
	EnvStatusIssuer   = "TREEBIND_STATUS_ISSUER"
	EnvStatusAudience = "TREEBIND_STATUS_AUDIENCE"
	EnvStatusSubject  = "TREEBIND_STATUS_SUBJECT"
	EnvStatusKeysFile = "TREEBIND_STATUS_KEYS_FILE"
)

// The defaults of the settings that have one.
const (
	DefaultMountPath  = "/work"
	DefaultStatusAddr = ":8080"
	DefaultStatusKeys = "/etc/treebind/status/jwks.json"
)

// Config is what the agent runs with.
type Config struct {
	// Namespace and Name identify the Treebind the agent works for.
	Namespace, Name string
	// MountPath is where the claim is mounted: the directory the agent writes to.
	MountPath string
	// SubPath is the directory inside the claim that is synced with the repository,
	// relative to MountPath. It is empty when the whole claim is.
	SubPath string

	// StatusAddr is the address the status endpoint and the probes listen on.
	StatusAddr string
	// StatusIssuer, StatusAudience and StatusSubject are the iss, aud and sub
	// claims that the operator's token has to carry.
	StatusIssuer, StatusAudience, StatusSubject string
	// StatusKeysFile is the file with the cluster's public signing keys.
	StatusKeysFile string
}

// FromEnv reads the settings with getenv, which is os.Getenv in the agent. It
// returns every problem it finds, not only the first, so that a pod with a wrong
// setup is put right in one go.
func FromEnv(getenv func(string) string) (Config, error) {
	// A value from a mounted file or a loose manifest can carry a trailing newline
	// or space, which would never match a claim.
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	config := Config{
		Namespace: get(EnvNamespace),
		Name:      get(EnvName),
		MountPath: orDefault(get(EnvMountPath), DefaultMountPath),
		// Not trimmed: the CRD accepts a path with spaces around it, the operator passes
		// it on as it is, and it names the directory the user meant.
		SubPath:        getenv(EnvSubPath),
		StatusAddr:     orDefault(get(EnvStatusAddr), DefaultStatusAddr),
		StatusIssuer:   get(EnvStatusIssuer),
		StatusAudience: get(EnvStatusAudience),
		StatusSubject:  get(EnvStatusSubject),
		StatusKeysFile: orDefault(get(EnvStatusKeysFile), DefaultStatusKeys),
	}
	return config, config.validate()
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// validate returns every problem with the settings.
func (c Config) validate() error {
	var problems []error
	require := func(env, value string) {
		if value == "" {
			problems = append(problems, fmt.Errorf("%s is not set", env))
		}
	}
	require(EnvNamespace, c.Namespace)
	require(EnvName, c.Name)
	require(EnvStatusIssuer, c.StatusIssuer)
	require(EnvStatusAudience, c.StatusAudience)
	require(EnvStatusSubject, c.StatusSubject)

	if !cleanAbsolute(c.MountPath) {
		problems = append(problems, fmt.Errorf("%s must be an absolute path with no .. and no trailing slash", EnvMountPath))
	}
	if err := CheckSubPath(c.SubPath); err != nil {
		problems = append(problems, fmt.Errorf("%s %w", EnvSubPath, err))
	}
	if !cleanAbsolute(c.StatusKeysFile) {
		problems = append(problems, fmt.Errorf("%s must be an absolute path with no .. and no trailing slash", EnvStatusKeysFile))
	}
	if !validAddress(c.StatusAddr) {
		problems = append(problems, fmt.Errorf("%s must be an address with a port from 0 to 65535, such as :8080", EnvStatusAddr))
	}
	// The subject is the operator's service account. A value that is not one can
	// never match a token, so the endpoint would refuse everyone.
	if c.StatusSubject != "" && !IsServiceAccount(c.StatusSubject) {
		problems = append(problems, fmt.Errorf("%s must be a service account, system:serviceaccount:<namespace>:<name>", EnvStatusSubject))
	}
	return errors.Join(problems...)
}

// MaxSubPathLength is the most characters that target.subPath of the CRD may have.
const MaxSubPathLength = 1024

// StateDirName is the directory at the root of the claim where the agent keeps its
// own state, the Git directory among it (docs/adr/0016). The synced directory
// cannot be it, or start with it.
const StateDirName = ".treebind"

// CheckSubPath returns the reason when a value cannot be the sub path of a Treebind,
// and nil when it can. An empty one is the whole claim. The rules are the ones that
// the CRD puts on target.subPath, and a test holds the two together: a value that the
// API server accepts and the agent refuses would leave a sync pod that exits at start
// for ever.
func CheckSubPath(subPath string) error {
	if subPath == "" {
		return nil
	}
	switch {
	case utf8.RuneCountInString(subPath) > MaxSubPathLength:
		return fmt.Errorf("must be at most %d characters", MaxSubPathLength)
	case strings.HasPrefix(subPath, "/"):
		return errors.New("must be relative")
	case subPath == ".":
		return errors.New("must not be '.'")
	case strings.Contains(subPath, `\`):
		return errors.New("must not contain a backslash")
	case strings.Contains(subPath, "//"):
		return errors.New("must not contain '//'")
	case strings.HasSuffix(subPath, "/"):
		return errors.New("must not end with '/'")
	}
	segments := strings.Split(subPath, "/")
	for _, segment := range segments {
		if segment == ".." || segment == "." || segment == ".git" {
			return errors.New("must not contain a '..', '.' or '.git' segment")
		}
	}
	if segments[0] == StateDirName {
		return fmt.Errorf("must not start with '%s', which the agent keeps for its own state", StateDirName)
	}
	return nil
}

// validAddress reports whether an address is a host, which may be empty, and a
// number for a port.
func validAddress(address string) bool {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number >= 0 && number <= 65535
}

// cleanAbsolute reports whether a path is absolute and already in its shortest
// form, with no .. in it, no double slash and no slash at the end.
func cleanAbsolute(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p
}

// IsServiceAccount reports whether a subject names a service account:
// system:serviceaccount:<namespace>:<name>. The operator checks the subject it
// gives the agent with the rule the agent checks it with.
func IsServiceAccount(subject string) bool {
	parts := strings.Split(subject, ":")
	return len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" && parts[2] != "" && parts[3] != ""
}
