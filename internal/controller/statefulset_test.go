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

package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The values more than one test uses.
const (
	testImage        = "example.com/treebind-agent:v1"
	testSSHKeyName   = "dashboards-deploy-key"
	testSSHKeyKey    = "id_ed25519"
	testHostsName    = "git-known-hosts"
	testHostsKey     = "known_hosts"
	testClaimName    = "grafana-data"
	testTreebindName = "grafana"
	testSyncName     = "grafana-sync"
	testCustomPath   = "/data/grafana"
	testNamespace    = "apps"
	testPodName      = "grafana-sync-0"
	// The field of the pod that the downward API gives its name from.
	fieldPodName = "metadata.name"
	testIssuer   = "https://kubernetes.default.svc.cluster.local"
	testSubject  = "system:serviceaccount:treebind-system:treebind-controller-manager"
)

func mustDesiredStatefulSet(t *testing.T, treebind *treebindv1alpha1.Treebind, image string) *appsv1.StatefulSet {
	t.Helper()
	statefulSet, err := desiredStatefulSet(treebind,
		agentSettings{Image: image, StatusIssuer: testIssuer, StatusSubject: testSubject})
	if err != nil {
		t.Fatalf("desiredStatefulSet() error = %v", err)
	}
	return statefulSet
}

func sshKeyRef() *treebindv1alpha1.KeySelector {
	return &treebindv1alpha1.KeySelector{Name: testSSHKeyName, Key: testSSHKeyKey}
}

func knownHostsRef() *treebindv1alpha1.KeySelector {
	return &treebindv1alpha1.KeySelector{Name: testHostsName, Key: testHostsKey}
}

func TestDesiredStatefulSetVolumes(t *testing.T) {
	// Not 0400: the kubelet gives Secret files to root, so a non-root agent
	// without an fsGroup could not read a key with that mode (ADR 0007).
	mode := int32(0o444)
	dataVolume := corev1.Volume{
		Name: dataVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: testClaimName},
		},
	}
	sshVolume := corev1.Volume{
		Name: sshVolumeName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  testSSHKeyName,
			Items:       []corev1.KeyToPath{{Key: testSSHKeyKey, Path: testSSHKeyKey}},
			DefaultMode: &mode,
		}},
	}
	hostsVolume := corev1.Volume{
		Name: knownHostsVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: testHostsName},
			Items:                []corev1.KeyToPath{{Key: testHostsKey, Path: testHostsKey}},
		}},
	}
	// The keys of the cluster come from a ConfigMap that may not exist yet.
	optional := true
	keysVolume := corev1.Volume{
		Name: statusKeysVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: testSyncName + "-keys"},
			Items:                []corev1.KeyToPath{{Key: "jwks.json", Path: "jwks.json"}},
			Optional:             &optional,
		}},
	}
	keysMount := corev1.VolumeMount{Name: statusKeysVolumeName, MountPath: "/etc/treebind/status", ReadOnly: true}
	sshMount := corev1.VolumeMount{Name: sshVolumeName, MountPath: sshMountPath, ReadOnly: true}
	hostsMount := corev1.VolumeMount{Name: knownHostsVolumeName, MountPath: knownHostsMountPath, ReadOnly: true}

	tests := []struct {
		name        string
		mountPath   string
		auth        *treebindv1alpha1.AuthSpec
		wantVolumes []corev1.Volume
		wantMounts  []corev1.VolumeMount
	}{
		{
			name:        "the claim is mounted at /work by default and nothing else is",
			wantVolumes: []corev1.Volume{dataVolume, keysVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}, keysMount},
		},
		{
			name:        "a custom mountPath is used as given",
			mountPath:   testCustomPath,
			wantVolumes: []corev1.Volume{dataVolume, keysVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: testCustomPath}, keysMount},
		},
		{
			name:        "a mountPath that is not in its shortest form is cleaned, as the agent needs it",
			mountPath:   "/data//grafana/",
			wantVolumes: []corev1.Volume{dataVolume, keysVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: testCustomPath}, keysMount},
		},
		{
			name:        "an empty auth block mounts nothing",
			auth:        &treebindv1alpha1.AuthSpec{},
			wantVolumes: []corev1.Volume{dataVolume, keysVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}, keysMount},
		},
		{
			name:        "an SSH key Secret is mounted read-only with mode 0444",
			auth:        &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()},
			wantVolumes: []corev1.Volume{dataVolume, keysVolume, sshVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}, keysMount, sshMount},
		},
		{
			name:        "a known hosts ConfigMap is mounted read-only",
			auth:        &treebindv1alpha1.AuthSpec{KnownHostsConfigMapRef: knownHostsRef()},
			wantVolumes: []corev1.Volume{dataVolume, keysVolume, hostsVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}, keysMount, hostsMount},
		},
		{
			name:        "both are mounted next to a custom mountPath",
			mountPath:   testCustomPath,
			auth:        &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef()},
			wantVolumes: []corev1.Volume{dataVolume, keysVolume, sshVolume, hostsVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: testCustomPath}, keysMount, sshMount, hostsMount},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Target.MountPath = tt.mountPath
			treebind.Spec.Repository.Auth = tt.auth

			pod := mustDesiredStatefulSet(t, treebind, testImage).Spec.Template.Spec
			if !reflect.DeepEqual(pod.Volumes, tt.wantVolumes) {
				t.Errorf("volumes = %+v, want %+v", pod.Volumes, tt.wantVolumes)
			}
			if got := pod.Containers[0].VolumeMounts; !reflect.DeepEqual(got, tt.wantMounts) {
				t.Errorf("volume mounts = %+v, want %+v", got, tt.wantMounts)
			}
		})
	}
}

// wantPodLabels and wantObjectLabels spell out the labels the objects of the
// Treebind with the given name must carry.
func wantPodLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "treebind",
		"app.kubernetes.io/instance":  name,
		"app.kubernetes.io/component": "sync-agent",
	}
}

func wantObjectLabels(name string) map[string]string {
	labels := wantPodLabels(name)
	labels["app.kubernetes.io/managed-by"] = "treebind-operator"
	return labels
}

func TestDesiredStatefulSetShape(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	statefulSet := mustDesiredStatefulSet(t, treebind, testImage)

	if statefulSet.Name != testSyncName || statefulSet.Namespace != treebind.Namespace {
		t.Errorf("name = %s/%s, want %s/%s", statefulSet.Namespace, statefulSet.Name, treebind.Namespace, testSyncName)
	}
	if want := wantObjectLabels(testTreebindName); !reflect.DeepEqual(statefulSet.Labels, want) {
		t.Errorf("labels = %v, want %v", statefulSet.Labels, want)
	}
	if want := wantObjectLabels(testTreebindName); !reflect.DeepEqual(statefulSet.Spec.Template.Labels, want) {
		t.Errorf("pod template labels = %v, want %v", statefulSet.Spec.Template.Labels, want)
	}
	if want := wantPodLabels(testTreebindName); !reflect.DeepEqual(statefulSet.Spec.Selector.MatchLabels, want) {
		t.Errorf("selector = %v, want %v", statefulSet.Spec.Selector.MatchLabels, want)
	}

	spec := statefulSet.Spec
	if spec.Replicas == nil || *spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", spec.Replicas)
	}
	if spec.ServiceName != testSyncName {
		t.Errorf("serviceName = %q, want %s", spec.ServiceName, testSyncName)
	}
	if spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		t.Errorf("updateStrategy = %q, want RollingUpdate", spec.UpdateStrategy.Type)
	}
	if spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Errorf("podManagementPolicy = %q, want Parallel", spec.PodManagementPolicy)
	}
	if len(spec.VolumeClaimTemplates) != 0 {
		t.Errorf("volumeClaimTemplates = %v, want none", spec.VolumeClaimTemplates)
	}
}

func TestDesiredStatefulSetAgentContainer(t *testing.T) {
	pod := mustDesiredStatefulSet(t, newTreebind(testTreebindName), testImage).Spec.Template.Spec

	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v, want runAsNonRoot and the RuntimeDefault seccomp profile",
			pod.SecurityContext)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	container := pod.Containers[0]
	if container.Name != agentContainerName || container.Image != testImage {
		t.Errorf("container = %s with image %s, want %s with image %s",
			container.Name, container.Image, agentContainerName, testImage)
	}
	if len(container.Command) != 0 || len(container.Args) != 0 {
		t.Errorf("command = %v, args = %v, want neither", container.Command, container.Args)
	}
	security := container.SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
		security.Capabilities == nil || !reflect.DeepEqual(security.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("container securityContext = %+v, want no privilege escalation, a read-only root and ALL dropped",
			security)
	}
	wantResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	if !reflect.DeepEqual(container.Resources, wantResources) {
		t.Errorf("resources = %+v, want %+v", container.Resources, wantResources)
	}
}

func TestTemplateHash(t *testing.T) {
	hashOf := func(mutate func(*treebindv1alpha1.Treebind), image string) string {
		t.Helper()
		treebind := newTreebind(testTreebindName)
		mutate(treebind)
		return mustDesiredStatefulSet(t, treebind, image).Annotations[templateHashAnnotation]
	}
	none := func(*treebindv1alpha1.Treebind) {}

	base := hashOf(none, testImage)
	if base == "" {
		t.Fatalf("annotation %s is empty", templateHashAnnotation)
	}

	tests := []struct {
		name       string
		mutate     func(*treebindv1alpha1.Treebind)
		image      string
		wantChange bool
	}{
		{name: "the same input gives the same hash", mutate: none, image: testImage},
		{
			name:   "the default mountPath and an explicit /work are the same template",
			mutate: func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.MountPath = defaultMountPath },
			image:  testImage,
		},
		{
			name:       "another image changes it",
			mutate:     none,
			image:      "example.com/treebind-agent:v2",
			wantChange: true,
		},
		{
			name:       "another mountPath changes it",
			mutate:     func(tb *treebindv1alpha1.Treebind) { tb.Spec.Target.MountPath = testCustomPath },
			image:      testImage,
			wantChange: true,
		},
		{
			name: "an SSH key Secret changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()}
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name: "the repository changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.URL = "https://example.com/other/repository.git"
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name: "the branch changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.Branch = "release"
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name: "the direction of the sync changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Sync.Direction = treebindv1alpha1.SyncDirectionPull
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name: "the interval of the sync changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Sync.Interval = &metav1.Duration{Duration: time.Minute}
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name:   "a spec.paths without lists leaves it alone",
			mutate: func(tb *treebindv1alpha1.Treebind) { tb.Spec.Paths = &treebindv1alpha1.PathsSpec{} },
			image:  testImage,
		},
		{
			name: "empty lists leave it alone",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{}, Exclude: []string{}}
			},
			image: testImage,
		},
		{
			name: "an include list changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{pathsJSON}}
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name: "an exclude list changes it",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Paths = &treebindv1alpha1.PathsSpec{Exclude: []string{pathsTmp}}
			},
			image:      testImage,
			wantChange: true,
		},
		{
			name: "settings the pod does not read leave it alone",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Sync.ConflictPolicy = treebindv1alpha1.ConflictPolicyFollowLocal
				tb.Spec.Sync.SettleTime = &metav1.Duration{Duration: time.Minute}
			},
			image: testImage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hashOf(tt.mutate, tt.image)
			if changed := got != base; changed != tt.wantChange {
				t.Errorf("hash changed = %v, want %v", changed, tt.wantChange)
			}
		})
	}
}

// A change of a list replaces the sync pod, and so does taking a list away.
func TestTemplateHashFollowsTheLists(t *testing.T) {
	states := []*treebindv1alpha1.PathsSpec{
		nil,
		{Include: []string{pathsJSON}},
		{Include: []string{pathsJSON, "*.yaml"}},
		{Include: []string{pathsJSON, "*.yaml"}, Exclude: []string{pathsTmp}},
		{Exclude: []string{pathsTmp}},
		{Exclude: []string{"*.bak"}},
		{Include: []string{pathsTmp}},
		nil,
	}
	hashes := make([]string, len(states))
	for i, paths := range states {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Paths = paths
		hashes[i] = mustDesiredStatefulSet(t, treebind, testImage).Annotations[templateHashAnnotation]
		if i > 0 && hashes[i] == hashes[i-1] {
			t.Errorf("going from %+v to %+v leaves the hash as it was, so the pod is not replaced", states[i-1], paths)
		}
	}
	if hashes[0] != hashes[len(hashes)-1] {
		t.Errorf("a Treebind whose lists are taken away has the hash %s, and %s before it had any",
			hashes[len(hashes)-1], hashes[0])
	}
}

// The pod template of a Treebind without lists is the one that the operator wrote before it passed
// the lists on, byte for byte, so that upgrading the operator replaces none of those sync pods
// (#87). The hashes are those of commit 2eb611c. A change that alters them replaces every such pod,
// and has to say so.
func TestATemplateWithoutListsIsTheOneOfTheOperatorBefore(t *testing.T) {
	full := func() *treebindv1alpha1.Treebind {
		treebind := newTreebind(testTreebindName)
		treebind.Namespace = testNamespace
		treebind.Spec.Target.MountPath = testCustomPath
		treebind.Spec.Target.SubPath = subPathNested
		treebind.Spec.Repository.URL = "ssh://git@example.com/acme/dashboards.git"
		treebind.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
			SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef(),
		}
		treebind.Spec.Sync.Interval = &metav1.Duration{Duration: 90 * time.Second}
		return treebind
	}
	for name, tt := range map[string]struct {
		treebind func() *treebindv1alpha1.Treebind
		want     string
	}{
		"a Treebind with the defaults": {
			func() *treebindv1alpha1.Treebind { return newTreebind(testTreebindName) },
			"8530124d950b17a4134515946055610afff6dea2dd08c84b58550abb27b8e656",
		},
		"a Treebind with every other field": {full, "7c7ba5bbc2ed57165528bb8803abda0ee01aecc379c79f2bf3ba7399e0a3a410"},
	} {
		for _, paths := range withoutLists() {
			treebind := tt.treebind()
			treebind.Spec.Paths = paths
			if got := mustDesiredStatefulSet(t, treebind, testImage).Annotations[templateHashAnnotation]; got != tt.want {
				t.Errorf("%s and spec.paths %+v: hash %s, want %s, which the operator wrote before. If the change "+
					"of the template is meant, upgrading the operator replaces every sync pod: say so, and pin the new hash",
					name, paths, got, tt.want)
			}
		}
	}
}

func TestDesiredService(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	service := desiredService(treebind)

	if service.Name != testSyncName || service.Namespace != treebind.Namespace {
		t.Errorf("name = %s/%s, want %s/%s", service.Namespace, service.Name, treebind.Namespace, testSyncName)
	}
	if service.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("clusterIP = %q, want None", service.Spec.ClusterIP)
	}
	if len(service.Spec.Ports) != 0 {
		t.Errorf("ports = %v, want none", service.Spec.Ports)
	}
	if !service.Spec.PublishNotReadyAddresses {
		t.Error("publishNotReadyAddresses = false: the name of the sync pod would not resolve until it is ready, " +
			"which it is not before its first sync, so the operator could not reach the agent")
	}
	statefulSet := mustDesiredStatefulSet(t, treebind, testImage)
	if !reflect.DeepEqual(service.Spec.Selector, statefulSet.Spec.Selector.MatchLabels) {
		t.Errorf("selector = %v, want the StatefulSet selector %v",
			service.Spec.Selector, statefulSet.Spec.Selector.MatchLabels)
	}
	if !reflect.DeepEqual(service.Labels, statefulSet.Labels) {
		t.Errorf("labels = %v, want %v", service.Labels, statefulSet.Labels)
	}
}

// agentContainer returns the agent container of a desired StatefulSet.
func agentContainer(t *testing.T, treebind *treebindv1alpha1.Treebind) (corev1.Container, corev1.PodSpec) {
	t.Helper()
	pod := mustDesiredStatefulSet(t, treebind, testImage).Spec.Template.Spec
	if len(pod.Containers) != 1 || pod.Containers[0].Name != agentContainerName {
		t.Fatalf("containers = %+v, want one called %s", pod.Containers, agentContainerName)
	}
	return pod.Containers[0], pod
}

func TestAgentGetsNoKubernetesCredential(t *testing.T) {
	_, pod := agentContainer(t, newTreebind(testTreebindName))

	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false: the agent holds no credential (ADR 0011)",
			pod.AutomountServiceAccountToken)
	}
}

func TestAgentEnvironment(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Namespace = testNamespace
	treebind.Spec.Target.MountPath = testCustomPath

	container, _ := agentContainer(t, treebind)

	want := []corev1.EnvVar{
		{Name: "TREEBIND_NAME", Value: testTreebindName},
		{Name: "TREEBIND_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
		{Name: "TREEBIND_MOUNT_PATH", Value: testCustomPath},
		{Name: "TREEBIND_REPO_URL", Value: httpsRepositoryURL},
		{Name: "TREEBIND_REPO_BRANCH", Value: branchMain},
		{Name: "TREEBIND_SYNC_DIRECTION", Value: "bidirectional"},
		{Name: "TREEBIND_POD_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: fieldPodName},
		}},
		{Name: "TREEBIND_STATUS_ADDR", Value: ":8080"},
		{Name: "TREEBIND_STATUS_ISSUER", Value: testIssuer},
		{Name: "TREEBIND_STATUS_AUDIENCE", Value: "treebind.example.org/" + testNamespace + "/" + testTreebindName},
		{Name: "TREEBIND_STATUS_SUBJECT", Value: testSubject},
		{Name: "TREEBIND_STATUS_KEYS_FILE", Value: "/etc/treebind/status/jwks.json"},
	}
	if !reflect.DeepEqual(container.Env, want) {
		t.Errorf("env = %+v\nwant %+v", container.Env, want)
	}
}

// With every setting the variables come in the order of the file, and the lists of spec.paths come
// as JSON after the name of the pod.
func TestAgentEnvironmentWithEverySetting(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Target.SubPath = subPathNested
	treebind.Spec.Repository.URL = sshRepositoryURL
	treebind.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
		SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef(),
	}
	treebind.Spec.Sync.Interval = &metav1.Duration{Duration: time.Minute}
	treebind.Spec.Paths = &treebindv1alpha1.PathsSpec{
		Include: []string{pathsJSON, "a<b>&c"}, Exclude: []string{pathsTmp, "$$x"},
	}

	container, _ := agentContainer(t, treebind)

	names := make([]string, 0, len(container.Env))
	for _, variable := range container.Env {
		names = append(names, variable.Name)
	}
	want := []string{
		agentconfig.EnvName, agentconfig.EnvNamespace, agentconfig.EnvMountPath, agentconfig.EnvSubPath,
		agentconfig.EnvRepoURL, agentconfig.EnvRepoBranch, agentconfig.EnvSyncInterval, agentconfig.EnvSyncDirection,
		agentconfig.EnvPodName, agentconfig.EnvPathsInclude, agentconfig.EnvPathsExclude, agentconfig.EnvSSHKeyFile,
		agentconfig.EnvKnownHostsFile, agentconfig.EnvStatusAddr, agentconfig.EnvStatusIssuer,
		agentconfig.EnvStatusAudience, agentconfig.EnvStatusSubject, agentconfig.EnvStatusKeysFile,
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("variables = %v\nwant        %v", names, want)
	}
	env := agentEnvironmentOf(container)
	if got, want := env[agentconfig.EnvPathsInclude], `["*.json","a\u003cb\u003e\u0026c"]`; got != want {
		t.Errorf("%s = %s, want %s", agentconfig.EnvPathsInclude, got, want)
	}
	if got, want := env[agentconfig.EnvPathsExclude], `["*.tmp","\u0024\u0024x"]`; got != want {
		t.Errorf("%s = %s, want %s", agentconfig.EnvPathsExclude, got, want)
	}
}

// The agent exits with code 2 on a setting it does not accept, and a restart does
// not cure that, so what the operator puts in the pod has to pass the agent's own
// checks. This reads the environment the way the kubelet gives it to the agent.
func TestTheAgentAcceptsTheEnvironmentItIsGiven(t *testing.T) {
	for name, mountPath := range map[string]string{
		"the default mount path": "",
		"a custom one":           testCustomPath,
		"one that is not clean":  "/work//data/",
	} {
		t.Run(name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Namespace = testNamespace
			treebind.Spec.Target.MountPath = mountPath
			container, _ := agentContainer(t, treebind)

			env := map[string]string{}
			for _, variable := range container.Env {
				if variable.ValueFrom != nil {
					env[variable.Name] = testNamespace // metadata.namespace, the only field the pod reads
					continue
				}
				env[variable.Name] = variable.Value
			}
			config, err := agentconfig.FromEnv(func(name string) string { return env[name] })

			if err != nil {
				t.Fatalf("the agent refuses the environment of its own pod: %v", err)
			}
			if config.Namespace != testNamespace || config.Name != testTreebindName ||
				config.StatusAudience != "treebind.example.org/"+testNamespace+"/"+testTreebindName {
				t.Errorf("the agent reads %+v", config)
			}
			// The mount path in the environment is where the volume is mounted.
			mounted := ""
			for _, mount := range container.VolumeMounts {
				if mount.Name == dataVolumeName {
					mounted = mount.MountPath
				}
			}
			if config.MountPath != mounted {
				t.Errorf("the agent writes to %s, the claim is mounted at %s", config.MountPath, mounted)
			}
		})
	}
}

func TestProbesAskThePortTheAgentListensOn(t *testing.T) {
	container, _ := agentContainer(t, newTreebind(testTreebindName))

	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != 8080 || container.Ports[0].Name != "status" {
		t.Fatalf("ports = %+v, want 8080 called status", container.Ports)
	}
	for name, tt := range map[string]struct {
		probe *corev1.Probe
		path  string
	}{
		"readiness": {container.ReadinessProbe, "/readyz"},
		"liveness":  {container.LivenessProbe, "/healthz"},
	} {
		if tt.probe == nil || tt.probe.HTTPGet == nil {
			t.Fatalf("the %s probe is not an HTTP probe: %+v", name, tt.probe)
		}
		get := tt.probe.HTTPGet
		if get.Path != tt.path || get.Port.String() != "status" {
			t.Errorf("the %s probe asks %s on %s, want %s on status", name, get.Path, get.Port.String(), tt.path)
		}
		if tt.probe.PeriodSeconds != 10 || tt.probe.TimeoutSeconds != 2 || tt.probe.FailureThreshold != 3 {
			t.Errorf("the %s probe timing is %+v", name, tt.probe)
		}
	}
}

func TestEveryAgentIsCalledWithItsOwnAudience(t *testing.T) {
	first, second := newTreebind("first"), newTreebind("second")
	other := newTreebind("first")
	other.Namespace = "elsewhere"

	audiences := map[string]bool{statusAudience(first): true, statusAudience(second): true, statusAudience(other): true}

	if len(audiences) != 3 {
		t.Errorf("two Treebinds share an audience: %v", audiences)
	}
	if want := "treebind.example.org/" + first.Namespace + "/first"; statusAudience(first) != want {
		t.Errorf("audience = %s, want %s", statusAudience(first), want)
	}
}

func TestTheOperatorWillNotStartAgentsThatCannotCheckItsToken(t *testing.T) {
	for name, reconciler := range map[string]*TreebindReconciler{
		"no issuer":  {StatusSubject: testSubject},
		"no subject": {StatusIssuer: testIssuer},
	} {
		// The manager is nil, so an error of the controller builder would pass for
		// the refusal: the text has to be the operator's own.
		if err := reconciler.SetupWithManager(nil); err == nil || !strings.Contains(err.Error(), "issuer and the subject") {
			t.Errorf("%s: SetupWithManager returned %v, want the refusal of the operator", name, err)
		}
	}
}

func TestNoPodTemplateForAgentsThatCannotCheckTheOperatorsToken(t *testing.T) {
	for name, agent := range map[string]agentSettings{
		"no issuer":  {Image: testImage, StatusSubject: testSubject},
		"no subject": {Image: testImage, StatusIssuer: testIssuer},
	} {
		if _, err := desiredStatefulSet(newTreebind(testTreebindName), agent); err == nil {
			t.Errorf("%s: a StatefulSet was built", name)
		}
	}
}
