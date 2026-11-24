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

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SyncDirection says which way changes flow between the repository and the
// volume.
// +kubebuilder:validation:Enum=pull;bidirectional
type SyncDirection string

const (
	// SyncDirectionPull mirrors the repository into the volume. Local edits are
	// never committed or pushed.
	SyncDirectionPull SyncDirection = "pull"
	// SyncDirectionBidirectional also commits the application's edits and
	// pushes them to the repository.
	SyncDirectionBidirectional SyncDirection = "bidirectional"
)

// ConflictPolicy says which side wins when the repository and the volume have
// both changed the same file.
// +kubebuilder:validation:Enum=followRemote;followLocal
type ConflictPolicy string

const (
	// ConflictPolicyFollowRemote keeps the repository's version.
	ConflictPolicyFollowRemote ConflictPolicy = "followRemote"
	// ConflictPolicyFollowLocal keeps the version on the volume.
	ConflictPolicyFollowLocal ConflictPolicy = "followLocal"
)

// PlacementMode says where the sync pod runs in relation to the application
// that uses the volume.
// +kubebuilder:validation:Enum=standalone;sidecar
type PlacementMode string

const (
	// PlacementModeStandalone runs the sync pod on its own, next to the
	// application's pods when the claim is ReadWriteOnce.
	PlacementModeStandalone PlacementMode = "standalone"
	// PlacementModeSidecar runs the sync container inside the application's own
	// pods, which is how a ReadWriteOncePod claim can be shared.
	PlacementModeSidecar PlacementMode = "sidecar"
)

// KeySelector names a key in a Secret or ConfigMap. The object is always in
// the same namespace as the Treebind, so a Treebind cannot read another
// namespace's data.
type KeySelector struct {
	// name is the name of the Secret or ConfigMap.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// key is the key inside the Secret or ConfigMap that holds the value.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// AuthSpec holds the credentials the agent uses to reach the repository. The
// Secret and ConfigMap are mounted into the agent pod, so the operator never
// reads their contents.
type AuthSpec struct {
	// sshKeySecretRef points to the Secret key that holds the private SSH key.
	// It is required when the repository url uses ssh://.
	// +optional
	SSHKeySecretRef *KeySelector `json:"sshKeySecretRef,omitempty"`

	// knownHostsConfigMapRef points to the ConfigMap key that holds the
	// known_hosts entries used to verify the Git host. It is required when the
	// repository url uses ssh://, because host keys are always verified.
	// +optional
	KnownHostsConfigMapRef *KeySelector `json:"knownHostsConfigMapRef,omitempty"`
}

// RepositorySpec describes the Git repository to sync with.
// +kubebuilder:validation:XValidation:rule="!isURL(self.url) || url(self.url).getScheme() != 'ssh' || (has(self.auth) && has(self.auth.sshKeySecretRef) && has(self.auth.knownHostsConfigMapRef))",message="an ssh:// repository needs auth.sshKeySecretRef and auth.knownHostsConfigMapRef; host keys are always verified"
type RepositorySpec struct {
	// url is the address of the repository, for example
	// ssh://git@github.com/acme/dashboards.git. The scheme must be explicit;
	// the scp-style git@host:path shorthand is not accepted. Credentials never
	// go in the URL: they are supplied through auth.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() in ['ssh', 'https', 'http']",message="url must use an explicit ssh://, https:// or http:// scheme; scp-style host:path is not accepted"
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'[\\x00-\\x20\\x7f]')",message="url must not contain whitespace or control characters"
	// +kubebuilder:validation:XValidation:rule="!self.matches('^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@]*:[^/@]*@')",message="url must not embed a password; supply credentials through auth"
	// +kubebuilder:validation:XValidation:rule="!self.matches('^https?://[^/]*@')",message="http and https urls must not embed credentials; supply credentials through auth"
	URL string `json:"url"`

	// branch is the branch to follow. The repository's default branch is used
	// when it is empty.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="!self.contains('..') && !self.contains('@{') && !self.startsWith('-') && !self.startsWith('/') && !self.endsWith('.lock') && !self.endsWith('/')",message="branch must not contain '..' or '@{', start with '-' or '/', or end with '.lock' or '/'"
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'[\\x00-\\x20\\x7f~^:?*\\[\\\\]') && !self.contains('//') && self != '@' && !self.matches(r'(^|/)\\.') && !self.endsWith('.')",message="branch must not contain whitespace, control characters, '~', '^', ':', '?', '*', '[' or a backslash, nor '//', a component that starts with '.', or a trailing '.'"
	Branch string `json:"branch,omitempty"`

	// path is a directory inside the repository. Only that directory is synced
	// with the volume: its files are the files of the synced directory, at the
	// root of it (target.subPath, or the whole claim), and nothing else of the
	// repository is written to the volume. The patterns of paths are relative to
	// it. The whole repository is used when it is empty. The branch has to have
	// the directory when the Treebind starts, so that a mistake in the name does
	// not make the agent commit the application's files into a new directory of
	// the repository. A commit of the agent changes the files of the directory
	// and nothing outside it.
	//
	// A volume that is synced with the whole repository is not changed to
	// sync only a directory of it, nor the other way round: the agent refuses
	// to start it, with every file where it is, and says how to keep what is
	// there and how to start again.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && self != '.' && !self.contains('\\\\') && !self.contains('//') && !('..' in self.split('/'))",message="path must be relative, must not be '.' and must not contain '..', '//' or a backslash"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith('/') && !('.' in self.split('/')) && !('.git' in self.split('/'))",message="path must not end with '/' or contain a '.' or '.git' segment"
	Path string `json:"path,omitempty"`

	// auth names the credentials for the repository.
	// +optional
	Auth *AuthSpec `json:"auth,omitempty"`
}

// TargetSpec describes the volume the files live on. claimName and subPath
// are immutable because together they name the synced directory; retargeting
// a live Treebind would mix the histories of two directories.
// +kubebuilder:validation:XValidation:rule="self.claimName == oldSelf.claimName",message="claimName is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.subPath) == has(oldSelf.subPath) && (!has(self.subPath) || self.subPath == oldSelf.subPath)",message="subPath is immutable"
type TargetSpec struct {
	// claimName is the name of an existing PersistentVolumeClaim in the same
	// namespace as the Treebind. It cannot be changed after creation.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ClaimName string `json:"claimName"`

	// subPath is a directory inside the claim. Only that directory is synced
	// with the repository. The whole claim is used when it is empty. It cannot
	// be changed after creation.
	//
	// The sync agent keeps its Git directory and the rest of its own state in a
	// directory called .treebind at the root of the claim. With a subPath it is
	// beside the synced directory, so an application that mounts the claim with
	// the same subPath does not see it. Without one it is inside the synced
	// directory, where the application can see it. So an application should mount
	// only the subPath, and the subPath cannot start with .treebind.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && self != '.' && !self.contains('\\\\') && !self.contains('//') && !('..' in self.split('/'))",message="subPath must be relative, must not be '.' and must not contain '..', '//' or a backslash"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith('/') && !('.' in self.split('/')) && !('.git' in self.split('/'))",message="subPath must not end with '/' or contain a '.' or '.git' segment"
	// +kubebuilder:validation:XValidation:rule="self.split('/')[0] != '.treebind'",message="subPath must not start with '.treebind', which the sync agent keeps for its own state"
	SubPath string `json:"subPath,omitempty"`

	// mountPath is where the claim is mounted inside the sync container. It
	// does not affect where the application mounts the claim.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && !('..' in self.split('/'))",message="mountPath must be an absolute path without '..'"
	MountPath string `json:"mountPath,omitempty"`
}

// SyncSpec controls when and in which direction files are synced.
type SyncSpec struct {
	// direction is pull for a read-only mirror, or bidirectional to also commit
	// and push the application's edits.
	// +required
	Direction SyncDirection `json:"direction"`

	// interval is how often the agent polls the repository for new commits.
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="interval must be at least 1s"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// maxDeletions is the most files that one update may remove from the directory. An update that
	// would remove more is not applied: the agent says so, and asks again at every interval. The
	// agent applies up to 100 when this is not set. Raise it to let through an update that removes
	// more files, or remove the files yourself. An update that would leave the repository with no
	// file at all is refused whatever this says.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	MaxDeletions *int32 `json:"maxDeletions,omitempty"`

	// settleTime is the quiet period after the last local edit before the agent
	// commits, so a file that is still being written is not committed half done.
	// +optional
	// +kubebuilder:default="5s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="settleTime must be at least 1s"
	SettleTime *metav1.Duration `json:"settleTime,omitempty"`

	// conflictPolicy says which side wins when both changed the same file.
	// +optional
	// +kubebuilder:default=followRemote
	ConflictPolicy ConflictPolicy `json:"conflictPolicy,omitempty"`
}

// CommitSpec controls the commits the agent creates from local edits. Every commit is by one author, who is also
// its committer, so that a commit of the agent is easy to tell from one by a person, and a name and an email are
// set together or not at all. The agent has a default for what is not set: the author treebind-bot, with an
// email that cannot exist, and the message "treebind: sync {{.Count}} file(s)". The agent is the judge of every
// value, and the rules here refuse what it could never use, so that the mistake is found when the Treebind is
// written, and not when the first commit fails (docs/adr/0031).
// +kubebuilder:validation:XValidation:rule="has(self.authorName) == has(self.authorEmail)",message="authorName and authorEmail must be set together"
type CommitSpec struct {
	// authorName is the name recorded as the author of every commit. Git drops a newline, '<' and '>' anywhere in a
	// name, and a space, a control character and any of . , : ; " ' \ from its ends, so a name with one of them is
	// refused: the commits would say another name than the one written here.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'[<>\\x00-\\x1f\\x7f]')",message="authorName must not contain '<', '>' or a control character such as a newline"
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'^[\\x00-\\x20.,:;\\x22\\x27\\x5c]|[\\x00-\\x20.,:;\\x22\\x27\\x5c]$')",message="authorName must not start or end with a space or one of . , : ; \" ' \\, which Git drops"
	AuthorName string `json:"authorName,omitempty"`

	// authorEmail is the email recorded as the author of every commit. It is one bare address, such as
	// bot@example.org, with no name and no comment.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:XValidation:rule="self.matches(r'^[^@\\s]+@[^@\\s]+$')",message="authorEmail must be an address of the form name@host, with no space"
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'[<>()\\x00-\\x1f\\x7f]')",message="authorEmail must be a bare address: no name, no comment, no '<' or '>' and no control character"
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'^[.,:;\\x22\\x27\\x5c]|[.,:;\\x22\\x27\\x5c]$')",message="authorEmail must not start or end with one of . , : ; \" ' \\, which Git drops"
	AuthorEmail string `json:"authorEmail,omitempty"`

	// messageTemplate is the commit message, as a Go text/template, with {{.Count}} for the number of files in
	// the commit. Only the built-in functions of the package are there, so a template cannot run a program or read
	// a file. The whitespace around the message is trimmed, and the first line is the subject.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:XValidation:rule="!self.matches(r'\\x00')",message="messageTemplate must not contain a NUL"
	MessageTemplate string `json:"messageTemplate,omitempty"`

	// maxPerHour is the most commits that the agent makes in an hour, the commits of the files that the application
	// changed and the commits that resolving a conflict makes, counted together. At the limit the agent makes no
	// more commits until one of the commits of the hour is an hour old: the changes stay on the volume, and the
	// Commit check says TooManyCommits. It is what stops a loop that the agent cannot tell from the application's own
	// work, such as an application that answers what a pull brings, from making commits for ever. Raise it when the
	// application really writes that often.
	// +optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxPerHour *int32 `json:"maxPerHour,omitempty"`
}

// PathsSpec limits which files of the synced directory the agent commits. The files of the
// repository are written to the volume as they are. A pattern is a line of a .gitignore file,
// relative to the synced directory: "*", "?", "[...]", "\" and "**" as Git reads them, with no
// negation and no comment, and with spaces kept. A pattern is refused when it starts with '/' or
// '!', has an empty, '.' or '..' segment, ends with '\' or has one before a '/', leaves a '['
// open in its segment, has a '[' or a '/' inside a class (classes like [:alpha:] are not
// supported), or has a '**' that is not a whole segment. Whatever the lists say, the agent never
// commits some files, such as those of .git and files named like secrets (docs/adr/0022).
type PathsSpec struct {
	// include lists the patterns of the files to commit. Every file is included when the
	// list is empty.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.map(p, size(p)).sum() <= 16384",message="the patterns of a list must have at most 16384 characters in all"
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('/') && !('..' in self.split('/'))",message="patterns must be relative and must not contain '..'"
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('!')",message="patterns must not start with '!': there is no negation; write '\\!' for a name that starts with '!'"
	// +kubebuilder:validation:items:XValidation:rule="!self.matches(r'\\x00')",message="patterns must not contain a NUL"
	// +kubebuilder:validation:items:XValidation:rule="!self.matches(r'(^|/)\\.?/|(^|/)\\.$')",message="patterns must not have an empty or '.' segment"
	// +kubebuilder:validation:items:XValidation:rule="self.matches(r'^(?:(?:\\*\\*|\\*?(?:(?:\\\\[^/]|\\[(?:[!^](?:\\]|\\\\[^/]|[^\\\\\\[\\]/])|\\]|\\\\[^/]|[^\\\\\\[\\]/!^])(?:\\\\[^/]|[^\\\\\\[\\]/])*\\]|[^\\\\\\[*/])\\*?)*)(?:/|$))+$')",message="patterns must close each '[' within its segment, must have no '[' or '/' in a class and no '\\' at the end or before a '/', and may have '**' only as a whole segment"
	Include []string `json:"include,omitempty"`

	// exclude lists the patterns of the files never to commit, for example build output.
	// An exclude wins over an include.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.map(p, size(p)).sum() <= 16384",message="the patterns of a list must have at most 16384 characters in all"
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('/') && !('..' in self.split('/'))",message="patterns must be relative and must not contain '..'"
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('!')",message="patterns must not start with '!': there is no negation; write '\\!' for a name that starts with '!'"
	// +kubebuilder:validation:items:XValidation:rule="!self.matches(r'\\x00')",message="patterns must not contain a NUL"
	// +kubebuilder:validation:items:XValidation:rule="!self.matches(r'(^|/)\\.?/|(^|/)\\.$')",message="patterns must not have an empty or '.' segment"
	// +kubebuilder:validation:items:XValidation:rule="self.matches(r'^(?:(?:\\*\\*|\\*?(?:(?:\\\\[^/]|\\[(?:[!^](?:\\]|\\\\[^/]|[^\\\\\\[\\]/])|\\]|\\\\[^/]|[^\\\\\\[\\]/!^])(?:\\\\[^/]|[^\\\\\\[\\]/])*\\]|[^\\\\\\[*/])\\*?)*)(?:/|$))+$')",message="patterns must close each '[' within its segment, must have no '[' or '/' in a class and no '\\' at the end or before a '/', and may have '**' only as a whole segment"
	Exclude []string `json:"exclude,omitempty"`
}

// PlacementSpec says where the sync pod runs. The field is accepted now and
// takes effect once the placement work lands; until then the operator reports
// a value that is not implemented in the SpecSupported condition.
type PlacementSpec struct {
	// mode is standalone to run the sync pod on its own, or sidecar to run the
	// sync container inside the application's pods.
	// +optional
	// +kubebuilder:default=standalone
	Mode PlacementMode `json:"mode,omitempty"`
}

// AgentSpec overrides how the sync pod is scheduled and run. Every field is
// accepted now and takes effect once the placement work lands; until then the
// operator ignores them and reports each one that is set in the SpecSupported
// condition.
type AgentSpec struct {
	// resources are the compute resources of the sync container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// nodeSelector restricts the nodes the sync pod can run on.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations lets the sync pod run on nodes with matching taints.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// priorityClassName is the priority class of the sync pod.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// imagePullSecrets names the Secrets in the Treebind's namespace used to
	// pull the agent image.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:XValidation:rule="size(self.name) > 0",message="imagePullSecrets entries need a name"
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// labels are added to the sync pod.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	Labels map[string]string `json:"labels,omitempty"`

	// annotations are added to the sync pod.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	Annotations map[string]string `json:"annotations,omitempty"`

	// runAsUser is the user ID the sync container runs as.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// runAsGroup is the group ID the sync container runs as.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`

	// fsGroup is the group ID that owns the mounted volume, so the sync
	// container can write to a claim that the application's user created.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FSGroup *int64 `json:"fsGroup,omitempty"`
}

// TreebindSpec defines the desired state of Treebind.
type TreebindSpec struct {
	// repository is the Git repository to sync with.
	// +required
	Repository RepositorySpec `json:"repository"`

	// target is the volume the files live on.
	// +required
	Target TargetSpec `json:"target"`

	// sync controls the direction and timing of the sync.
	// +required
	Sync SyncSpec `json:"sync"`

	// commit controls the commits created from local edits.
	// +optional
	Commit *CommitSpec `json:"commit,omitempty"`

	// paths limits which files are synced.
	// +optional
	Paths *PathsSpec `json:"paths,omitempty"`

	// placement says where the sync pod runs. It is defaulted as a whole so a
	// Treebind without it reads back as standalone.
	// +optional
	// +kubebuilder:default={mode: standalone}
	Placement PlacementSpec `json:"placement,omitzero"`

	// agent overrides how the sync pod is scheduled and run.
	// +optional
	Agent *AgentSpec `json:"agent,omitempty"`
}

// TreebindStatus defines the observed state of Treebind.
type TreebindStatus struct {
	// observedGeneration is the generation of the spec that the status
	// describes. A status older than metadata.generation is not up to date.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastSyncedCommit is the commit of the repository at which the sync agent
	// last found the volume and the repository in agreement, after a pull or a
	// push. It is the full id of the commit of the repository, also when
	// spec.repository.path syncs only one directory of it, and it is absent until
	// they have agreed once. A step that fails, a restart of the sync pod and a
	// poll that does not get the report change nothing here: the Synced condition
	// says whether they agree now.
	// +optional
	LastSyncedCommit string `json:"lastSyncedCommit,omitempty"`

	// lastSyncTime is when the volume and the repository came to agree at
	// lastSyncedCommit. It moves when lastSyncedCommit does, and not at every
	// pull that finds nothing new, so a Treebind with nothing to sync keeps it.
	// Whether the report is current is what the AgentUnreachable and Synced
	// conditions say.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// conditions represent the current state of the Treebind resource. Each
	// condition has a unique type. The status of each condition is one of True,
	// False, or Unknown.
	//
	// SpecSupported and Ready are set by the reconciler. Synced and Conflict are
	// set by the status poller from the sync agent's reports, and are Unknown
	// until the first report arrives. AgentUnreachable is set by the status poller
	// and says whether the operator can read the agent's report. It does not
	// change Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	// ConditionSpecSupported is the type of the condition that says whether the
	// operator implements every field set in the spec.
	ConditionSpecSupported = "SpecSupported"

	// ReasonSupported means every field set in the spec is implemented.
	ReasonSupported = "Supported"
	// ReasonNotImplemented means the spec sets a field that is accepted but
	// not implemented yet, so the operator ignores it.
	ReasonNotImplemented = "NotImplemented"

	// ConditionReady is the type of the condition that says whether the sync
	// pod is running and ready.
	ConditionReady = "Ready"

	// ReasonSyncPodReady means the sync StatefulSet reports its pod ready.
	ReasonSyncPodReady = "SyncPodReady"
	// ReasonSyncPodNotReady means the sync StatefulSet has no ready pod.
	ReasonSyncPodNotReady = "SyncPodNotReady"
	// ReasonSyncPodOOMKilled means the sync pod is not ready because its container
	// was killed for using more memory than its limit. The message names the limit.
	ReasonSyncPodOOMKilled = "SyncPodOOMKilled"
	// ReasonNotSynced means the sync pod is ready but the volume is not in sync
	// with the repository. The message names the reason of the Synced condition.
	ReasonNotSynced = "NotSynced"
	// ReasonReconcileFailed means the operator could not create or update the
	// sync objects. The message carries the error, and the sync pod count says
	// nothing until a reconcile succeeds.
	ReasonReconcileFailed = "ReconcileFailed"

	// ConditionSynced is the type of the condition that says whether the volume
	// matches the repository at status.lastSyncedCommit. It is True when no step
	// of the sync has failed, False with the reason of the step that failed, and
	// Unknown while the operator cannot tell, which its reasons say. The reasons of
	// False are the ones the agent reports for its steps, spelled as it spells them,
	// and are API in the sense of docs/adr/0004: users and tooling match on them.
	ConditionSynced = "Synced"

	// ConditionConflict is the type of the condition that says whether the last pull
	// found a conflict between the volume and the repository that it could not
	// resolve, so that local work waits for someone. It is True with the reason of the
	// update that could not be made, False when there is no such conflict, and Unknown
	// as Synced is. A Treebind that only pulls has no conflict to resolve: an update
	// that it cannot take is Synced False. It never changes Ready.
	ConditionConflict = "Conflict"

	// ReasonAwaitingAgentReport means the sync agent has not reported yet, so the
	// condition is Unknown.
	ReasonAwaitingAgentReport = "AwaitingAgentReport"
	// ReasonAgentUnreachable means the operator has not been able to read the report
	// of the agent for the time that AgentUnreachable waits, so it does not know what
	// the sync does. The condition is Unknown.
	ReasonAgentUnreachable = "AgentUnreachable"
	// ReasonSyncPending means the report has no outcome of the sync to give yet: the
	// lock, the checkout or the first pull is still to come, as it is after every start
	// of the sync pod. The condition is Unknown.
	ReasonSyncPending = "SyncPending"
	// ReasonAgentOutdated means the report is of an agent that runs another pod
	// template than the spec gives now, as the sync pod does while it is replaced, so it
	// says nothing of the sync of this spec. The condition is Unknown.
	ReasonAgentOutdated = "AgentOutdated"
	// ReasonInSync means no step of the sync failed and the volume and the repository
	// agree at status.lastSyncedCommit. The condition is True.
	ReasonInSync = "InSync"
	// ReasonSyncFailed means a step of the sync failed with a reason that this release
	// of the operator does not define, as an agent of another release can report, so
	// that the operator writes no reason it did not define. The condition is False, and
	// the log of the sync pod says which step and why.
	ReasonSyncFailed = "SyncFailed"
	// ReasonNoConflict means the last pull found no conflict, or the Treebind only
	// pulls, which has none to resolve. The Conflict condition is False.
	ReasonNoConflict = "NoConflict"
	// ReasonConflictResolved means the last conflict that the agent found since it
	// started was resolved by spec.sync.conflictPolicy, including one by followRemote,
	// which keeps the volume's version of a file in a ref that is never pushed. The
	// Conflict condition is False.
	ReasonConflictResolved = "ConflictResolved"

	// The reasons that follow are the reasons of a step of the sync that failed, spelled
	// as the sync agent spells them in its report, and a test holds them to the agent's
	// (docs/adr/0009). Synced is False with the reason of a step that failed, and Conflict
	// is True with the reason of the pull when it could not resolve a conflict.

	// ReasonMountPathInvalid means the path where the volume is mounted does not exist,
	// cannot be read or is not a directory.
	ReasonMountPathInvalid = "MountPathInvalid"
	// ReasonNotMounted means the path is on the container's own filesystem and not on a
	// mounted volume.
	ReasonNotMounted = "NotMounted"
	// ReasonNotWritable means the user of the sync agent cannot create a file in the
	// directory.
	ReasonNotWritable = "NotWritable"
	// ReasonReadOnlyVolume means the volume refuses writes.
	ReasonReadOnlyVolume = "ReadOnlyVolume"
	// ReasonWriteFailed means writing to the volume failed for another reason, such as a
	// full volume or an I/O error.
	ReasonWriteFailed = "WriteFailed"
	// ReasonGitDirUnsafe means the Git directory, the state directory or a directory on
	// the way to the synced directory is a link, belongs to another user or is open to
	// others, or the Git directory holds something the agent did not put there, such as a
	// hook or a config key, or the agent cannot look at it to tell. Git is not run against
	// it, and the directory is left as it is.
	ReasonGitDirUnsafe = "GitDirUnsafe"
	// ReasonTimedOut means the volume did not answer in time.
	ReasonTimedOut = "TimedOut"
	// ReasonLockFailed means the lock that keeps one agent writing to the volume could
	// not be taken: the volume refused it, or it is held in a format that a newer release
	// wrote. The agent tries again with a growing wait.
	ReasonLockFailed = "LockFailed"
	// ReasonDirectoryNotEmpty means the directory holds files that are not the branch's,
	// and the agent does not write over data it did not put there. Nothing was touched.
	ReasonDirectoryNotEmpty = "DirectoryNotEmpty"
	// ReasonRepositoryMismatch means the Git directory that is there is of another
	// repository, or follows another branch than the one the Treebind names.
	ReasonRepositoryMismatch = "RepositoryMismatch"
	// ReasonBranchNotFound means the repository has no such branch, or no default branch
	// to follow.
	ReasonBranchNotFound = "BranchNotFound"
	// ReasonReservedPath means the branch has a file where the agent keeps its own state,
	// and a checkout or an update would write it there.
	ReasonReservedPath = "ReservedPath"
	// ReasonFetchFailed means a command that asks the repository failed: the host cannot
	// be reached, the key is refused, or the repository does not exist.
	ReasonFetchFailed = "FetchFailed"
	// ReasonCheckoutFailed means the agent could not make the checkout on the volume.
	ReasonCheckoutFailed = "CheckoutFailed"
	// ReasonPathNotFound means the branch has no directory at spec.repository.path, or has
	// something there that is not one. Nothing was written.
	ReasonPathNotFound = "PathNotFound"
	// ReasonPathMismatch means the checkout was made for another directory than the one
	// spec.repository.path names now, or for the whole repository, and a checkout is not
	// changed to the other. Nothing was written.
	ReasonPathMismatch = "PathMismatch"
	// ReasonBlocked means the update changes a file that has a local edit, or that an
	// untracked file is in the way of. Nothing was written, and the next pull tries again.
	ReasonBlocked = "Blocked"
	// ReasonDiverged means the volume and the repository each have commits that the other
	// lacks, and the Treebind has no conflict policy to say which wins because it only
	// pulls. Nothing was written.
	ReasonDiverged = "Diverged"
	// ReasonBranchGone means the repository has no such branch any more.
	ReasonBranchGone = "BranchGone"
	// ReasonTooManyDeletions means the update would remove more files from the directory
	// than spec.sync.maxDeletions allows at once, or every file of the branch. Nothing was
	// written, and the agent asks again at every interval.
	ReasonTooManyDeletions = "TooManyDeletions"
	// ReasonRefused means the agent stopped the update itself, as when it lost the lock
	// while the update ran.
	ReasonRefused = "Refused"
	// ReasonTooManyCommits means a commit is not made because the agent has made as many
	// in the last hour as spec.commit.maxPerHour allows. The files stay on the volume, or
	// the conflict stays unresolved, until one of the commits of the hour is an hour old.
	ReasonTooManyCommits = "TooManyCommits"
	// ReasonPullFailed means the agent could not pull for another reason: Git failed on
	// the volume, or the checkout is not what a pull expects.
	ReasonPullFailed = "PullFailed"
	// ReasonScanFailed means the agent could not look for the files that changed on the
	// volume.
	ReasonScanFailed = "ScanFailed"
	// ReasonCommitFailed means a commit could not be made: Git said no, the lock could not
	// be checked, or the volume refused a write. The files stay on the volume, and the next
	// scan finds them again.
	ReasonCommitFailed = "CommitFailed"
	// ReasonNonFastForward means the repository has commits on the branch that the volume
	// lacks, so the push is not a fast-forward. The agent does not force it: the next pull
	// brings the commits, or the conflict policy says which side wins.
	ReasonNonFastForward = "NonFastForward"
	// ReasonPushRejected means the repository refused the push: a protected branch, a hook,
	// a rule, a file that is too large, a secret that it found. A person puts it right.
	ReasonPushRejected = "PushRejected"
	// ReasonPushFailed means the push could not be made: the repository did not answer, the
	// key was refused, the volume refused, or the lock could not be checked. The agent tries
	// again with a growing wait.
	ReasonPushFailed = "PushFailed"

	// ConditionAgentUnreachable is the type of the condition that says whether the
	// operator can read the report of the sync agent. It is False while it can,
	// True once it has not been able to for a few minutes, and Unknown while it
	// cannot tell. It is about the operator's view of the agent and not about the
	// sync: the sync can work while the operator cannot read it, so it never changes
	// Ready.
	ConditionAgentUnreachable = "AgentUnreachable"

	// ReasonReportReceived means the agent answered with a report. The condition
	// is False.
	ReasonReportReceived = "ReportReceived"
	// ReasonConnectionFailed means the connection to the sync pod was refused or
	// nothing answered. The condition is True. A network policy or a service mesh
	// that does not let the operator through looks like this.
	ReasonConnectionFailed = "ConnectionFailed"
	// ReasonTokenRefused means the agent answered 401 to the operator's token. The
	// condition is True.
	ReasonTokenRefused = "TokenRefused"
	// ReasonUnexpectedResponse means the agent answered with a status that is
	// neither a report nor a refusal. The condition is True.
	ReasonUnexpectedResponse = "UnexpectedResponse"
	// ReasonReportUnreadable means the agent answered with something that is not a
	// report the operator can use. The condition is True.
	ReasonReportUnreadable = "ReportUnreadable"
	// ReasonPollFailing means the polls have been failing for less than the time
	// it takes to call it. The condition is Unknown.
	ReasonPollFailing = "PollFailing"
	// ReasonAgentNotRunning means the name of the sync pod does not resolve, which
	// is a pod that does not exist yet. The condition is Unknown, and Ready says
	// more.
	ReasonAgentNotRunning = "AgentNotRunning"
	// ReasonTokenUnavailable means the operator could not get a token for the
	// agent, which is not the agent's fault. The condition is Unknown.
	ReasonTokenUnavailable = "TokenUnavailable"
	// ReasonKeysUnavailable means the operator could not put the cluster's public
	// keys where the agent reads them, so the agent refuses its token. The
	// condition is Unknown.
	ReasonKeysUnavailable = "KeysUnavailable"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Conflict",type=string,JSONPath=`.status.conditions[?(@.type=="Conflict")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(self.metadata.name) || self.metadata.name.size() <= 56",message="metadata.name must be at most 56 characters, so the sync pod name, the name plus '-sync-0', fits the 63-character DNS label limit"

// Treebind is the Schema for the treebinds API
type Treebind struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Treebind
	// +required
	Spec TreebindSpec `json:"spec"`

	// status defines the observed state of Treebind
	// +optional
	Status TreebindStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TreebindList contains a list of Treebind
type TreebindList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Treebind `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Treebind{}, &TreebindList{})
		return nil
	})
}
