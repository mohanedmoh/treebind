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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
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
)

func mustDesiredStatefulSet(t *testing.T, treebind *treebindv1alpha1.Treebind, image string) *appsv1.StatefulSet {
	t.Helper()
	statefulSet, err := desiredStatefulSet(treebind, image)
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
	mode := int32(0o400)
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
			wantVolumes: []corev1.Volume{dataVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}},
		},
		{
			name:        "a custom mountPath is used as given",
			mountPath:   testCustomPath,
			wantVolumes: []corev1.Volume{dataVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: testCustomPath}},
		},
		{
			name:        "an empty auth block mounts nothing",
			auth:        &treebindv1alpha1.AuthSpec{},
			wantVolumes: []corev1.Volume{dataVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}},
		},
		{
			name:        "an SSH key Secret is mounted read-only with mode 0400",
			auth:        &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef()},
			wantVolumes: []corev1.Volume{dataVolume, sshVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}, sshMount},
		},
		{
			name:        "a known hosts ConfigMap is mounted read-only",
			auth:        &treebindv1alpha1.AuthSpec{KnownHostsConfigMapRef: knownHostsRef()},
			wantVolumes: []corev1.Volume{dataVolume, hostsVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: defaultMountPath}, hostsMount},
		},
		{
			name:        "both are mounted next to a custom mountPath",
			mountPath:   testCustomPath,
			auth:        &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef()},
			wantVolumes: []corev1.Volume{dataVolume, sshVolume, hostsVolume},
			wantMounts:  []corev1.VolumeMount{{Name: dataVolumeName, MountPath: testCustomPath}, sshMount, hostsMount},
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
	wantEnv := []corev1.EnvVar{
		{Name: "TREEBIND_NAME", Value: testTreebindName},
		{Name: "TREEBIND_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
	}
	if !reflect.DeepEqual(container.Env, wantEnv) {
		t.Errorf("env = %+v, want %+v", container.Env, wantEnv)
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
			name: "settings the pod does not read leave it alone",
			mutate: func(tb *treebindv1alpha1.Treebind) {
				tb.Spec.Repository.Branch = "release"
				tb.Spec.Sync.Direction = treebindv1alpha1.SyncDirectionPull
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
	statefulSet := mustDesiredStatefulSet(t, treebind, testImage)
	if !reflect.DeepEqual(service.Spec.Selector, statefulSet.Spec.Selector.MatchLabels) {
		t.Errorf("selector = %v, want the StatefulSet selector %v",
			service.Spec.Selector, statefulSet.Spec.Selector.MatchLabels)
	}
	if !reflect.DeepEqual(service.Labels, statefulSet.Labels) {
		t.Errorf("labels = %v, want %v", service.Labels, statefulSet.Labels)
	}
}
