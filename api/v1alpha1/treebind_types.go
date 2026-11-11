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
	// +optional
	SSHKeySecretRef *KeySelector `json:"sshKeySecretRef,omitempty"`

	// knownHostsConfigMapRef points to the ConfigMap key that holds the
	// known_hosts entries used to verify the Git host.
	// +optional
	KnownHostsConfigMapRef *KeySelector `json:"knownHostsConfigMapRef,omitempty"`
}

// RepositorySpec describes the Git repository to sync with.
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
	// with the volume. The whole repository is used when it is empty.
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
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && self != '.' && !self.contains('\\\\') && !self.contains('//') && !('..' in self.split('/'))",message="subPath must be relative, must not be '.' and must not contain '..', '//' or a backslash"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith('/') && !('.' in self.split('/')) && !('.git' in self.split('/'))",message="subPath must not end with '/' or contain a '.' or '.git' segment"
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

// CommitSpec controls the commits the agent creates from local edits.
type CommitSpec struct {
	// authorName is the name recorded as the author of every commit.
	// +optional
	AuthorName string `json:"authorName,omitempty"`

	// authorEmail is the email recorded as the author of every commit.
	// +optional
	AuthorEmail string `json:"authorEmail,omitempty"`

	// messageTemplate is the commit message, as a Go text/template.
	// +optional
	MessageTemplate string `json:"messageTemplate,omitempty"`
}

// PathsSpec limits which files are synced, using glob patterns relative to the
// synced directory.
type PathsSpec struct {
	// include lists the patterns of files to sync. Everything is included when
	// the list is empty.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('/') && !('..' in self.split('/'))",message="patterns must be relative and must not contain '..'"
	Include []string `json:"include,omitempty"`

	// exclude lists the patterns of files to leave out, for example temporary
	// and swap files.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=512
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('/') && !('..' in self.split('/'))",message="patterns must be relative and must not contain '..'"
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

	// lastSyncedCommit is the commit the volume and the repository last agreed
	// on.
	// +optional
	LastSyncedCommit string `json:"lastSyncedCommit,omitempty"`

	// lastSyncTime is when the last sync finished.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// conditions represent the current state of the Treebind resource. Each
	// condition has a unique type and is set by the reconciler. The status of
	// each condition is one of True, False, or Unknown.
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
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
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
