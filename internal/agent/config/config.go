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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
)

// The names of the environment variables.
const (
	EnvName      = "TREEBIND_NAME"
	EnvNamespace = "TREEBIND_NAMESPACE"
	EnvMountPath = "TREEBIND_MOUNT_PATH"
	// EnvSubPath is the directory of the claim that is synced, relative to the mount
	// path. It is not set when the whole claim is (docs/adr/0016).
	EnvSubPath = "TREEBIND_SUB_PATH"
	// EnvRepoURL is the address of the repository, and EnvRepoBranch the branch to follow. The
	// branch is not set when the repository's default branch is.
	EnvRepoURL    = "TREEBIND_REPO_URL"
	EnvRepoBranch = "TREEBIND_REPO_BRANCH"
	// EnvRepoPath is the directory of the repository that is the synced directory: spec.repository.path (docs/adr/0036). It is not
	// set when the whole repository is, so that a pod template that never had it does not change.
	EnvRepoPath = "TREEBIND_REPO_PATH"
	// EnvSSHKeyFile and EnvKnownHostsFile are the files, in the pod, of the SSH key and of the
	// host keys that are trusted. They are paths and never the content (NFR-3), and they are
	// set when the Treebind names the Secret and the ConfigMap they come from.
	EnvSSHKeyFile     = "TREEBIND_SSH_KEY_FILE"
	EnvKnownHostsFile = "TREEBIND_KNOWN_HOSTS_FILE"
	// EnvSyncInterval is how often the agent pulls, as a Go duration such as 30s. The default is
	// the one of the CRD when it is not set. EnvSyncDirection is pull or bidirectional.
	EnvSyncInterval  = "TREEBIND_SYNC_INTERVAL"
	EnvSyncDirection = "TREEBIND_SYNC_DIRECTION"
	// EnvSyncSettleTime is how long a file has to be quiet before the agent commits it, as a Go
	// duration such as 5s. The default is the one of the CRD when it is not set.
	EnvSyncSettleTime = "TREEBIND_SYNC_SETTLE_TIME"
	// EnvSyncMaxDeletions is the most files that one update may remove from the directory, as a whole
	// number. It is not set when the Treebind does not say, and the pull has a cap of its own then
	// (docs/adr/0027).
	EnvSyncMaxDeletions = "TREEBIND_SYNC_MAX_DELETIONS"
	// EnvSyncConflictPolicy is which side wins when the checkout and the repository each have commits that the other
	// lacks: followRemote or followLocal (docs/adr/0034). It is not set for followRemote, the default of the CRD, so that
	// a pod template that never had it does not change, and the agent has the default of the CRD for it.
	EnvSyncConflictPolicy = "TREEBIND_SYNC_CONFLICT_POLICY"
	// EnvPodName is the name of the sync pod. The writer lock names its holder with it.
	EnvPodName = "TREEBIND_POD_NAME"
	// EnvPathsInclude and EnvPathsExclude are the patterns of spec.paths, each list as a JSON
	// array of strings. A list that is empty is not set (docs/adr/0022).
	EnvPathsInclude = "TREEBIND_PATHS_INCLUDE"
	EnvPathsExclude = "TREEBIND_PATHS_EXCLUDE"
	// EnvCommitAuthorName and EnvCommitAuthorEmail are the author and the committer of every commit, and
	// EnvCommitMessageTemplate is the template of its message, as spec.commit says them. Each is not set when
	// the spec does not say it, and the agent has a default for it (docs/adr/0031).
	EnvCommitAuthorName      = "TREEBIND_COMMIT_AUTHOR_NAME"
	EnvCommitAuthorEmail     = "TREEBIND_COMMIT_AUTHOR_EMAIL"
	EnvCommitMessageTemplate = "TREEBIND_COMMIT_MESSAGE_TEMPLATE"
	// EnvCommitMaxPerHour is the most commits that the agent makes in an hour, as a whole number: spec.commit.maxPerHour
	// (docs/adr/0035). It is not set for the default of the CRD, so that a pod template that never had it does not change, and
	// the agent has the default of the CRD for it.
	EnvCommitMaxPerHour = "TREEBIND_COMMIT_MAX_PER_HOUR"
	// EnvTemplateHash is the hash of the pod template that the agent runs in, which the operator computes over the
	// template without this variable, so that it is a function of the rest of it. The agent only reports it, so that the
	// operator can tell which template a report is of. A pod from before the operator set it does not have it.
	EnvTemplateHash = "TREEBIND_TEMPLATE_HASH"

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

	// DefaultSyncInterval is the interval of the CRD, which a test holds to the default that the API
	// server fills in.
	DefaultSyncInterval = 30 * time.Second
	// MinSyncInterval is the shortest interval the CRD admits.
	MinSyncInterval = time.Second
	// DefaultSettleTime and MinSettleTime are the default and the least of spec.sync.settleTime, which a
	// test holds to what the API server does.
	DefaultSettleTime = 5 * time.Second
	MinSettleTime     = time.Second
	// MinMaxDeletions and MaxMaxDeletions are the least and the most that the CRD admits for
	// spec.sync.maxDeletions, which a test holds to what the API server does.
	MinMaxDeletions = 1
	MaxMaxDeletions = math.MaxInt32
	// DefaultCommitMaxPerHour is the default of spec.commit.maxPerHour, which a test holds to the default that the API
	// server fills in, and MinCommitMaxPerHour and MaxCommitMaxPerHour are the least and the most that the CRD admits. The
	// breaker has the same three, and a test holds them to each other.
	DefaultCommitMaxPerHour = breaker.DefaultLimit
	MinCommitMaxPerHour     = breaker.MinLimit
	MaxCommitMaxPerHour     = breaker.MaxLimit
)

// The directions of a sync, as the CRD names them.
const (
	DirectionPull          = "pull"
	DirectionBidirectional = "bidirectional"
)

// The conflict policies, as the CRD names them (docs/adr/0034). followRemote is the default of the CRD and of the agent.
const (
	ConflictFollowRemote = "followRemote"
	ConflictFollowLocal  = "followLocal"
	// DefaultConflictPolicy is the policy of a Treebind that does not say, which a test holds to the default of the CRD.
	DefaultConflictPolicy = ConflictFollowRemote
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

	// RepoURL is the address of the repository, and RepoBranch the branch to follow, which
	// is empty for the default branch of the repository.
	RepoURL, RepoBranch string
	// RepoPath is the directory of the repository whose files are the synced directory, with its files at the root of it. It is
	// empty when the whole repository is.
	RepoPath string
	// SSHKeyFile and KnownHostsFile are the files of the SSH key and of the trusted host
	// keys. Both are set for an ssh:// repository. They are ignored for an https:// one.
	SSHKeyFile, KnownHostsFile string

	// SyncInterval is how often the agent pulls, and SyncDirection says whether it also commits
	// and pushes the application's edits: pull, or bidirectional.
	SyncInterval  time.Duration
	SyncDirection string
	// ConflictPolicy says which side wins when the checkout and the repository each gained commits: followRemote, which
	// is the default, or followLocal. Only a bidirectional sync has commits of its own to resolve.
	ConflictPolicy string
	// SettleTime is how long a file has to be quiet before the agent commits it.
	SettleTime time.Duration
	// MaxDeletions is the most files that one update may remove from the directory, or zero when the
	// Treebind does not say.
	MaxDeletions int
	// PodName is the name of the sync pod.
	PodName string
	// PathsInclude and PathsExclude are the include and exclude patterns of spec.paths, which
	// paths.Check takes. They are empty when the spec has none.
	PathsInclude, PathsExclude []string
	// CommitAuthorName, CommitAuthorEmail and CommitMessageTemplate are what spec.commit says, as they are
	// written, and are empty when it does not say. They are not judged here: the commit package does that when
	// it makes its committer, so that a wrong one is a failed check in the report and not an agent that exits.
	CommitAuthorName, CommitAuthorEmail, CommitMessageTemplate string
	// CommitMaxPerHour is the most commits that the agent makes in an hour, of the commit step and of the resolutions together
	// (docs/adr/0035). It is the default when the Treebind does not say.
	CommitMaxPerHour int
	// TemplateHash is the hash of the pod template that the agent runs in, as the operator computed it, or empty for a pod
	// that was started without one.
	TemplateHash string

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
	interval, intervalErr := CheckSyncInterval(get(EnvSyncInterval))
	settleTime, settleTimeErr := CheckSettleTime(get(EnvSyncSettleTime))
	maxDeletions, maxDeletionsErr := CheckMaxDeletions(get(EnvSyncMaxDeletions))
	commitMaxPerHour, commitMaxPerHourErr := CheckCommitMaxPerHour(get(EnvCommitMaxPerHour))
	include, includeErr := readPatterns(EnvPathsInclude, get(EnvPathsInclude))
	exclude, excludeErr := readPatterns(EnvPathsExclude, get(EnvPathsExclude))
	config := Config{
		Namespace: get(EnvNamespace),
		Name:      get(EnvName),
		MountPath: orDefault(get(EnvMountPath), DefaultMountPath),
		// Not trimmed: the CRD accepts a path with spaces around it, the operator passes
		// it on as it is, and it names the directory the user meant.
		SubPath:    getenv(EnvSubPath),
		RepoURL:    get(EnvRepoURL),
		RepoBranch: get(EnvRepoBranch),
		// Not trimmed, as the sub path is not: the CRD accepts a path with spaces around it, and the operator passes it as it is.
		RepoPath:       getenv(EnvRepoPath),
		SSHKeyFile:     get(EnvSSHKeyFile),
		KnownHostsFile: get(EnvKnownHostsFile),
		SyncInterval:   interval,
		SettleTime:     settleTime,
		SyncDirection:  get(EnvSyncDirection),
		ConflictPolicy: orDefault(get(EnvSyncConflictPolicy), DefaultConflictPolicy),
		MaxDeletions:   maxDeletions,
		PodName:        get(EnvPodName),
		PathsInclude:   include,
		PathsExclude:   exclude,
		// Not trimmed: spaces at the ends of a name are something that Git would drop, and the commit package
		// says so, and one at the ends of a template is the template's.
		CommitAuthorName:      getenv(EnvCommitAuthorName),
		CommitAuthorEmail:     getenv(EnvCommitAuthorEmail),
		CommitMessageTemplate: getenv(EnvCommitMessageTemplate),
		CommitMaxPerHour:      commitMaxPerHour,
		TemplateHash:          get(EnvTemplateHash),
		StatusAddr:            orDefault(get(EnvStatusAddr), DefaultStatusAddr),
		StatusIssuer:          get(EnvStatusIssuer),
		StatusAudience:        get(EnvStatusAudience),
		StatusSubject:         get(EnvStatusSubject),
		StatusKeysFile:        orDefault(get(EnvStatusKeysFile), DefaultStatusKeys),
	}
	err := config.validate()
	if intervalErr != nil {
		err = errors.Join(fmt.Errorf("%s %w", EnvSyncInterval, intervalErr), err)
	}
	if settleTimeErr != nil {
		err = errors.Join(fmt.Errorf("%s %w", EnvSyncSettleTime, settleTimeErr), err)
	}
	if maxDeletionsErr != nil {
		err = errors.Join(fmt.Errorf("%s %w", EnvSyncMaxDeletions, maxDeletionsErr), err)
	}
	if commitMaxPerHourErr != nil {
		err = errors.Join(fmt.Errorf("%s %w", EnvCommitMaxPerHour, commitMaxPerHourErr), err)
	}
	return config, errors.Join(err, includeErr, excludeErr)
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
	require(EnvRepoURL, c.RepoURL)
	require(EnvPodName, c.PodName)
	require(EnvSyncDirection, c.SyncDirection)
	if c.SyncDirection != "" {
		if err := CheckSyncDirection(c.SyncDirection); err != nil {
			problems = append(problems, fmt.Errorf("%s %w", EnvSyncDirection, err))
		}
	}
	if err := CheckConflictPolicy(c.ConflictPolicy); err != nil {
		problems = append(problems, fmt.Errorf("%s %w", EnvSyncConflictPolicy, err))
	}

	if !cleanAbsolute(c.MountPath) {
		problems = append(problems, fmt.Errorf("%s must be an absolute path with no .. and no trailing slash", EnvMountPath))
	}
	if err := CheckSubPath(c.SubPath); err != nil {
		problems = append(problems, fmt.Errorf("%s %w", EnvSubPath, err))
	}
	if err := CheckBranch(c.RepoBranch); err != nil {
		problems = append(problems, fmt.Errorf("%s %w", EnvRepoBranch, err))
	}
	if err := CheckRepoPath(c.RepoPath); err != nil {
		problems = append(problems, fmt.Errorf("%s %w", EnvRepoPath, err))
	}
	if err := CheckTemplateHash(c.TemplateHash); err != nil {
		problems = append(problems, fmt.Errorf("%s %w", EnvTemplateHash, err))
	}
	problems = append(problems, c.checkRepository()...)
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
	if err := checkRelativeDirectory(subPath, MaxSubPathLength); err != nil {
		return err
	}
	if subPath != "" && strings.Split(subPath, "/")[0] == StateDirName {
		return fmt.Errorf("must not start with '%s', which the agent keeps for its own state", StateDirName)
	}
	return nil
}

// MaxRepoPathLength is the most characters that repository.path of the CRD may have.
const MaxRepoPathLength = 1024

// CheckRepoPath returns the reason when a value cannot be the repository path of a Treebind, and nil when it can. An empty
// one is the whole repository. The rules are the ones that the CRD puts on repository.path, and a test holds the two together,
// for the reason given at CheckSubPath. They are the rules of the sub path except for the state directory: a directory of the
// repository becomes the root of the synced directory (docs/adr/0036), so it cannot reach the state directory by its name.
func CheckRepoPath(repoPath string) error {
	return checkRelativeDirectory(repoPath, MaxRepoPathLength)
}

// checkRelativeDirectory is what a sub path and a repository path have in common: a directory, relative, in its shortest form,
// that is not or does not reach the Git directory of a repository. An empty one says no directory.
func checkRelativeDirectory(dir string, longest int) error {
	if dir == "" {
		return nil
	}
	switch {
	case utf8.RuneCountInString(dir) > longest:
		return fmt.Errorf("must be at most %d characters", longest)
	case strings.HasPrefix(dir, "/"):
		return errors.New("must be relative")
	case dir == ".":
		return errors.New("must not be '.'")
	case strings.Contains(dir, `\`):
		return errors.New("must not contain a backslash")
	case strings.Contains(dir, "//"):
		return errors.New("must not contain '//'")
	case strings.HasSuffix(dir, "/"):
		return errors.New("must not end with '/'")
	}
	for segment := range strings.SplitSeq(dir, "/") {
		if segment == ".." || segment == "." || segment == ".git" {
			return errors.New("must not contain a '..', '.' or '.git' segment")
		}
	}
	return nil
}

// CheckSyncInterval returns the interval a value stands for, DefaultSyncInterval when it is empty,
// or the reason it cannot be one. The rules are the ones that the CRD puts on sync.interval, and a
// test holds the two together: an interval that the API server accepts and the agent refuses would
// leave a sync pod that exits at start for ever.
func CheckSyncInterval(raw string) (time.Duration, error) {
	return checkDuration(raw, DefaultSyncInterval, MinSyncInterval, "30s or 5m")
}

// CheckSettleTime returns the settle time a value stands for, DefaultSettleTime when it is empty, or the
// reason it cannot be one. The rules are the ones that the CRD puts on sync.settleTime, and a test holds
// the two together, for the reason given at CheckSyncInterval.
func CheckSettleTime(raw string) (time.Duration, error) {
	return checkDuration(raw, DefaultSettleTime, MinSettleTime, "5s or 1m")
}

// checkDuration returns the duration of a value, the default when it is empty, or the reason it cannot be
// one: it is not a Go duration, with examples of one, or it is shorter than the least.
func checkDuration(raw string, def, least time.Duration, examples string) (time.Duration, error) {
	if raw == "" {
		return def, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("must be a duration such as %s", examples)
	}
	if value < least {
		return 0, fmt.Errorf("must be at least %s", least)
	}
	return value, nil
}

// CheckMaxDeletions returns the cap that a value stands for, zero when it is empty, or the reason it
// cannot be one. The rules are the ones that the CRD puts on sync.maxDeletions: a whole number from
// MinMaxDeletions to MaxMaxDeletions. A test holds the two together, because a value that the API server
// accepts and the agent refuses would leave a sync pod that exits at start for ever.
func CheckMaxDeletions(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	// Digits and nothing else, in 31 bits, which is MaxMaxDeletions: the operator writes the number as Go
	// does, and a sign, an exponent, a prefix or a separator is a value that it never makes.
	value, err := strconv.ParseUint(raw, 10, 31)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return 0, fmt.Errorf("must be at most %d", MaxMaxDeletions)
	case err != nil:
		return 0, errors.New("must be a whole number")
	case value < MinMaxDeletions:
		return 0, fmt.Errorf("must be at least %d", MinMaxDeletions)
	}
	return int(value), nil
}

// CheckCommitMaxPerHour returns the limit that a value stands for, DefaultCommitMaxPerHour when it is empty, or the reason it
// cannot be one. The rules are the ones that the CRD puts on commit.maxPerHour: a whole number from MinCommitMaxPerHour to
// MaxCommitMaxPerHour. A test holds the two together, because a value that the API server accepts and the agent refuses would
// leave a sync pod that exits at start for ever.
func CheckCommitMaxPerHour(raw string) (int, error) {
	if raw == "" {
		return DefaultCommitMaxPerHour, nil
	}
	// Digits and nothing else, as the operator writes the number: a sign, an exponent, a prefix or a separator is a value that it
	// never makes. 31 bits is far more than the most, and a number that does not fit in them is too large.
	value, err := strconv.ParseUint(raw, 10, 31)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return 0, fmt.Errorf("must be at most %d", MaxCommitMaxPerHour)
	case err != nil:
		return 0, errors.New("must be a whole number")
	case value < MinCommitMaxPerHour:
		return 0, fmt.Errorf("must be at least %d", MinCommitMaxPerHour)
	case value > MaxCommitMaxPerHour:
		return 0, fmt.Errorf("must be at most %d", MaxCommitMaxPerHour)
	}
	return int(value), nil
}

// TemplateHashLength is the number of hexadecimal digits of the hash of a pod template, which is a SHA-256.
const TemplateHashLength = 64

// CheckTemplateHash returns the reason when a value cannot be the hash of a pod template, and nil when it can. An empty
// one says that the pod was started without a hash. It is the hash as the operator writes it, in lowercase, because the
// operator compares what the agent reports with its own by equality: a value of another spelling would be the report
// of no template for as long as the agent runs, and this way the agent says so once, at start.
func CheckTemplateHash(hash string) error {
	if hash == "" {
		return nil
	}
	if len(hash) == TemplateHashLength && !strings.ContainsFunc(hash, func(c rune) bool {
		return (c < '0' || c > '9') && (c < 'a' || c > 'f')
	}) {
		return nil
	}
	return fmt.Errorf("must be %d lowercase hexadecimal digits", TemplateHashLength)
}

// CheckSyncDirection returns the reason a value cannot be the direction of a sync. They are the
// values that the CRD admits for sync.direction.
func CheckSyncDirection(direction string) error {
	if direction != DirectionPull && direction != DirectionBidirectional {
		return fmt.Errorf("must be %s or %s", DirectionPull, DirectionBidirectional)
	}
	return nil
}

// CheckConflictPolicy returns the reason a value cannot be the conflict policy of a sync. They are the values that the
// CRD admits for sync.conflictPolicy.
func CheckConflictPolicy(policy string) error {
	if policy != ConflictFollowRemote && policy != ConflictFollowLocal {
		return fmt.Errorf("must be %s or %s", ConflictFollowRemote, ConflictFollowLocal)
	}
	return nil
}

// checkRepository returns the problems with the settings of the repository that are not
// the branch's: the address, and the files that an ssh:// repository needs.
func (c Config) checkRepository() []error {
	var problems []error
	if c.RepoURL != "" {
		if _, err := CheckRepositoryURL(c.RepoURL); err != nil {
			problems = append(problems, fmt.Errorf("%s %w", EnvRepoURL, err))
		}
	}
	// An ssh:// address needs its files whether or not the rest of it parses, so that the
	// problems are all said at once.
	ssh := strings.HasPrefix(strings.ToLower(c.RepoURL), SchemeSSH+":")
	for _, file := range []struct{ env, value string }{
		{EnvSSHKeyFile, c.SSHKeyFile}, {EnvKnownHostsFile, c.KnownHostsFile},
	} {
		if file.value != "" && !cleanAbsolute(file.value) {
			problems = append(problems, fmt.Errorf("%s must be an absolute path with no .. and no trailing slash", file.env))
		}
		if ssh && file.value == "" {
			problems = append(problems, fmt.Errorf("%s is not set, and an ssh:// repository needs it: host keys are always verified", file.env))
		}
	}
	return problems
}

// The schemes of a repository that the agent can sync with. The CRD admits http:// too, and
// the Runner refuses it, because Git over plain http sends everything in the clear (NFR-15).
const (
	SchemeSSH   = "ssh"
	SchemeHTTPS = "https"
)

// CheckRepositoryURL returns the scheme of the address of a repository, or the reason it
// cannot be synced with. It is not stricter than the CRD about anything but the scheme: a
// value that the API server accepts and the agent refuses leaves a sync pod that exits at
// start for ever, and the one value that does, http://, is the one the agent says so about.
func CheckRepositoryURL(raw string) (string, error) {
	for _, c := range raw {
		if c <= ' ' || c == 0x7f {
			return "", errors.New("must not contain whitespace or control characters")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("must be a URL with an explicit scheme, such as ssh:// or https://")
	}
	switch parsed.Scheme {
	case SchemeSSH, SchemeHTTPS:
		return parsed.Scheme, nil
	case "http":
		return "", errors.New("must not use http://: Git over plain http sends everything in the clear, and the agent " +
			"refuses it (NFR-15). Use https://")
	default:
		return "", errors.New("must use ssh:// or https://")
	}
}

// MaxBranchLength is the most characters that repository.branch of the CRD may have.
const MaxBranchLength = 255

// CheckBranch returns the reason when a value cannot be the branch of a Treebind, and
// nil when it can. An empty one is the default branch of the repository. The rules are
// the ones that the CRD puts on repository.branch, and a name that Git would take for
// an option, a revision range or a pattern is one of those they keep out: the branch
// goes into the refs and the refspec of the commands the agent runs.
func CheckBranch(branch string) error {
	if branch == "" {
		return nil
	}
	switch {
	case utf8.RuneCountInString(branch) > MaxBranchLength:
		return fmt.Errorf("must be at most %d characters", MaxBranchLength)
	case strings.Contains(branch, "..") || strings.Contains(branch, "@{") || branch == "@":
		return errors.New("must not contain '..' or '@{', or be '@'")
	case strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/"):
		return errors.New("must not start with '-' or '/'")
	case strings.HasSuffix(branch, ".lock") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, "."):
		return errors.New("must not end with '.lock', '/' or '.'")
	case strings.Contains(branch, "//"):
		return errors.New("must not contain '//'")
	}
	for _, c := range branch {
		if c <= ' ' || c == 0x7f || strings.ContainsRune("~^:?*[\\", c) {
			return errors.New("must not contain whitespace, control characters, '~', '^', ':', '?', '*', '[' or a backslash")
		}
	}
	for component := range strings.SplitSeq(branch, "/") {
		if strings.HasPrefix(component, ".") {
			return errors.New("must not have a component that starts with '.'")
		}
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

// The limits of a list of spec.paths, as in the CRD. MaxPathPatternsLength is the most
// characters that the patterns of a list may have in all. JSON writes a character in six
// bytes at most, so the longest list fits in one environment variable, which Linux limits
// to 128 KiB, with nearly 32 KiB to spare.
const (
	MaxPathPatterns       = 64
	MaxPathPatternsLength = 16384
)

// readPatterns reads a list of patterns of spec.paths from the environment variable env, a
// JSON array of strings. A list that is not set is empty.
func readPatterns(env, raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	// JSON reads a byte that is not UTF-8 as U+FFFD without a word, which would be a
	// pattern that the spec does not have. It reads an escape of half a UTF-16 pair, such
	// as \ud800, the same way, which the operator never writes: it writes the value from
	// the spec, which is UTF-8.
	if !utf8.ValidString(raw) {
		return nil, fmt.Errorf("%s must be UTF-8", env)
	}
	var patterns []string
	if err := json.Unmarshal([]byte(raw), &patterns); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of strings: %w", env, err)
	}
	if err := CheckPathPatterns(patterns); err != nil {
		return nil, fmt.Errorf("%s %w", env, err)
	}
	return patterns, nil
}

// CheckPathPatterns returns the reason a list cannot be spec.paths.include or
// spec.paths.exclude, and nil when it can. The rules are the ones that the CRD puts on the
// lists and on their patterns, which are those of paths.Check, and a test holds the two
// together: a list that the API server accepts and the agent refuses would leave a sync pod
// that exits at start for ever.
func CheckPathPatterns(patterns []string) error {
	if len(patterns) > MaxPathPatterns {
		return fmt.Errorf("must have at most %d patterns", MaxPathPatterns)
	}
	length := 0
	for _, pattern := range patterns {
		if err := paths.Check(pattern); err != nil {
			return fmt.Errorf("has a pattern, %q, that %w", pattern, err)
		}
		length += utf8.RuneCountInString(pattern)
	}
	if length > MaxPathPatternsLength {
		return fmt.Errorf("must have at most %d characters in all, and has %d", MaxPathPatternsLength, length)
	}
	return nil
}
