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
	"path"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/passwd"
)

// arbitraryUserIDSettings are the settings of an operator that is told to run the sync pods under any user ID.
func arbitraryUserIDSettings() agentSettings {
	return agentSettings{Image: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject, ArbitraryUserID: true}
}

// passwdTreebinds are a Treebind as it comes and one with an SSH key, which has more volumes.
func passwdTreebinds() map[string]*treebindv1alpha1.Treebind {
	withAuth := newTreebind(testTreebindName)
	withAuth.Spec.Target.MountPath = testCustomPath
	withAuth.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{
		SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef(),
	}
	return map[string]*treebindv1alpha1.Treebind{
		"a Treebind as it comes":     newTreebind(testTreebindName),
		"a Treebind with an SSH key": withAuth,
	}
}

// Without the setting the pod template is the one of the operator before it, which
// TestATemplateWithoutListsIsTheOneOfTheOperatorBefore holds byte for byte: no init container, no
// emptyDir, which kubectl drain refuses without --delete-emptydir-data, and no working directory.
func TestWithoutTheSettingTheSyncPodHasNoPasswdFile(t *testing.T) {
	for name, treebind := range passwdTreebinds() {
		pod := mustDesiredStatefulSet(t, treebind, testImage).Spec.Template
		if len(pod.Spec.InitContainers) != 0 {
			t.Errorf("%s: init containers = %+v, want none", name, pod.Spec.InitContainers)
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.EmptyDir != nil {
				t.Errorf("%s: volume %s is an emptyDir, want none", name, volume.Name)
			}
		}
		for _, mount := range pod.Spec.Containers[0].VolumeMounts {
			if mount.MountPath == passwd.SystemFile {
				t.Errorf("%s: %s is mounted, want the image's", name, passwd.SystemFile)
			}
		}
		if pod.Spec.Containers[0].WorkingDir != "" || len(pod.Annotations) != 0 {
			t.Errorf("%s: working directory %q and annotations %v, want neither", name,
				pod.Spec.Containers[0].WorkingDir, pod.Annotations)
		}
	}
}

// With the setting, an init container of the agent image writes a passwd file with an entry for the user of the pod
// into a volume, and the sync container sees that file at /etc/passwd (docs/adr/0042). ssh refuses a user that has no
// passwd entry, and the operator cannot know the user ID: the platform assigns it when the pod is admitted.
func TestWithTheSettingTheSyncPodWritesAPasswdEntryForItsUserBeforeTheAgentStarts(t *testing.T) {
	for name, treebind := range passwdTreebinds() {
		t.Run(name, func(t *testing.T) {
			statefulSet, err := desiredStatefulSet(treebind, arbitraryUserIDSettings())
			if err != nil {
				t.Fatal(err)
			}
			template := statefulSet.Spec.Template
			pod := template.Spec
			if len(pod.InitContainers) != 1 {
				t.Fatalf("init containers = %d, want 1", len(pod.InitContainers))
			}
			init, agent := pod.InitContainers[0], pod.Containers[0]

			if init.Name != passwdContainerName || init.Image != testImage {
				t.Errorf("init container = %s with image %s, want %s with image %s",
					init.Name, init.Image, passwdContainerName, testImage)
			}
			// The image's entrypoint is the agent, which the arguments make write the file and exit.
			if len(init.Command) != 0 {
				t.Errorf("command = %v, want none", init.Command)
			}
			if want := []string{passwd.Command, "/run/treebind/passwd"}; !reflect.DeepEqual(init.Args, want) {
				t.Errorf("args = %v, want %v", init.Args, want)
			}
			if len(init.Env) != 0 || len(init.Ports) != 0 || init.ReadinessProbe != nil || init.LivenessProbe != nil {
				t.Errorf("init container = %+v, want no environment, no port and no probe", init)
			}
			wantInitMounts := []corev1.VolumeMount{{Name: passwdVolumeName, MountPath: "/run/treebind"}}
			if !reflect.DeepEqual(init.VolumeMounts, wantInitMounts) {
				t.Errorf("init volume mounts = %+v, want %+v", init.VolumeMounts, wantInitMounts)
			}
			// It runs as the sync container does, so that the platform gives both the same user, and asks for no
			// more than the sync container, so that the pod asks for no more than without it.
			if !reflect.DeepEqual(init.SecurityContext, agent.SecurityContext) || agent.SecurityContext == nil {
				t.Errorf("init securityContext = %+v, want the one of the sync container, %+v",
					init.SecurityContext, agent.SecurityContext)
			}
			if !reflect.DeepEqual(init.Resources, agent.Resources) {
				t.Errorf("init resources = %+v, want the ones of the sync container, %+v", init.Resources, agent.Resources)
			}
			// The image's working directory, /home/git, is for UID 65532 alone, and a runtime may change to it
			// after it changes the user.
			if init.WorkingDir != "/" || agent.WorkingDir != "/" {
				t.Errorf("working directories = %q and %q, want / for both", init.WorkingDir, agent.WorkingDir)
			}

			var mount *corev1.VolumeMount
			for i := range agent.VolumeMounts {
				if agent.VolumeMounts[i].MountPath == passwd.SystemFile {
					mount = &agent.VolumeMounts[i]
				}
			}
			wantMount := corev1.VolumeMount{Name: passwdVolumeName, MountPath: passwd.SystemFile, SubPath: passwdFile, ReadOnly: true}
			if mount == nil || !reflect.DeepEqual(*mount, wantMount) {
				t.Fatalf("the mount at %s = %+v, want %+v", passwd.SystemFile, mount, wantMount)
			}
			// The file that the init container writes is the one that the sync container mounts.
			if written := path.Join(wantInitMounts[0].MountPath, mount.SubPath); written != init.Args[1] {
				t.Errorf("the sync container mounts %s of the volume, and the init container writes %s", written, init.Args[1])
			}

			sizeLimit := resource.MustParse("1Mi")
			wantVolume := corev1.Volume{
				Name:         passwdVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit}},
			}
			if last := pod.Volumes[len(pod.Volumes)-1]; !reflect.DeepEqual(last, wantVolume) {
				t.Errorf("last volume = %+v, want %+v", last, wantVolume)
			}
			// The cluster autoscaler does not take a node down that has a pod with an emptyDir, unless the pod says
			// that it may lose it. The file is written again at every start.
			want := map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes": passwdVolumeName}
			if !reflect.DeepEqual(template.Annotations, want) {
				t.Errorf("pod annotations = %v, want %v", template.Annotations, want)
			}

			// What the operator has without the setting is all there, in its order, before what the setting adds.
			off := mustDesiredStatefulSet(t, treebind, testImage).Spec.Template.Spec
			if !reflect.DeepEqual(pod.Volumes[:len(pod.Volumes)-1], off.Volumes) {
				t.Errorf("volumes = %+v, want %+v and the passwd volume", pod.Volumes, off.Volumes)
			}
			if !reflect.DeepEqual(agent.VolumeMounts[:len(agent.VolumeMounts)-1], off.Containers[0].VolumeMounts) {
				t.Errorf("volume mounts = %+v, want %+v and the passwd file", agent.VolumeMounts, off.Containers[0].VolumeMounts)
			}
		})
	}
}

// Turning the setting on or off is a change of the pod, which replaces it, and the agent is told the hash of the
// template that it has, so that the operator takes its reports for the ones of the current template.
func TestTheSettingChangesTheTemplateHashAndThePollerFollows(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	off := mustDesiredStatefulSet(t, treebind, testImage)
	on, err := desiredStatefulSet(treebind, arbitraryUserIDSettings())
	if err != nil {
		t.Fatal(err)
	}
	if off.Annotations[templateHashAnnotation] == on.Annotations[templateHashAnnotation] ||
		templateHashVariable(t, off) == templateHashVariable(t, on) {
		t.Error("the setting leaves the hash of the template as it was, so the pod is not replaced")
	}

	reconciler := &TreebindReconciler{
		AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject, AgentArbitraryUserID: true,
	}
	poller := &StatusPollerReconciler{
		AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject, AgentArbitraryUserID: true,
	}
	if reconciler.agentSettings() != arbitraryUserIDSettings() || poller.agentSettings() != reconciler.agentSettings() {
		t.Errorf("the reconcile gives %+v and the poller %+v, want both %+v",
			reconciler.agentSettings(), poller.agentSettings(), arbitraryUserIDSettings())
	}
}
