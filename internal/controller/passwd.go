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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/mohanedmoh/treebind/internal/agent/passwd"
)

// The passwd file of the sync container (docs/adr/0042). The init container passwdContainerName writes it, as the
// user that the platform gives the pod, to passwdFile in the volume, which it mounts at passwdDir, and the sync
// container mounts that file over /etc/passwd. ssh refuses to run for a user that has no entry.
const (
	passwdContainerName = "passwd"
	passwdVolumeName    = "passwd"
	passwdDir           = "/run/treebind"
	passwdFile          = "passwd"
	// passwdSizeLimit is far more than the file, which is the one of the image and a line.
	passwdSizeLimit = "1Mi"
	// safeToEvictLocalVolumes names the emptyDir volumes of a pod that the cluster autoscaler may lose when it takes
	// a node down. Without it, a pod with an emptyDir keeps its node up.
	safeToEvictLocalVolumes = "cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes"
	// rootDir is the working directory of both containers. The image's own, /home/git, is mode 0700 for UID 65532,
	// and a runtime may change to it after it changes the user, which another user ID cannot.
	rootDir = "/"
)

// withPasswdFile gives the sync pod of the template the passwd file of the user that it runs as, which the operator
// does when it is told to run the sync pods under any user ID: an init container of the agent image, with the
// security context and the resources of the sync container, writes it into an emptyDir, and the sync container mounts
// it read-only at /etc/passwd. The volume and the mount come after the others, so that they keep their order.
func withPasswdFile(template *corev1.PodTemplateSpec, image string) {
	spec := &template.Spec
	agent := &spec.Containers[0]
	agent.WorkingDir = rootDir
	agent.VolumeMounts = append(agent.VolumeMounts, corev1.VolumeMount{
		Name: passwdVolumeName, MountPath: passwd.SystemFile, SubPath: passwdFile, ReadOnly: true,
	})
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            passwdContainerName,
		Image:           image,
		Args:            []string{passwd.Command, path.Join(passwdDir, passwdFile)},
		WorkingDir:      rootDir,
		VolumeMounts:    []corev1.VolumeMount{{Name: passwdVolumeName, MountPath: passwdDir}},
		Resources:       *agent.Resources.DeepCopy(),
		SecurityContext: agent.SecurityContext.DeepCopy(),
	})
	size := resource.MustParse(passwdSizeLimit)
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         passwdVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}},
	})
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[safeToEvictLocalVolumes] = passwdVolumeName
}
