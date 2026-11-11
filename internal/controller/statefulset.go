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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

const (
	// templateHashAnnotation records the hash of the pod template the operator
	// last wrote to the StatefulSet.
	templateHashAnnotation = "treebind.example.org/template-hash"

	defaultMountPath    = "/work"
	sshMountPath        = "/etc/treebind/ssh"
	knownHostsMountPath = "/etc/treebind/known-hosts"

	agentContainerName   = "agent"
	dataVolumeName       = "data"
	sshVolumeName        = "ssh-key"
	knownHostsVolumeName = "known-hosts"

	// sshKeyFileMode is the mode of the SSH key in the pod. The kubelet creates
	// Secret files owned by root, and the agent runs as a non-root user without
	// an fsGroup, so a stricter mode would leave the agent unable to read its key.
	// OpenSSH rejects a key that is readable by the group or others only when
	// the user running it owns the file, and root does, so 0444 is accepted.
	// See docs/adr/0007.
	sshKeyFileMode int32 = 0o444
)

// syncName is the name of the headless Service and of the StatefulSet of a
// Treebind.
func syncName(treebind *treebindv1alpha1.Treebind) string {
	return treebind.Name + "-sync"
}

// podLabels are the labels that select the sync pod. They are immutable on a
// StatefulSet, so they must never depend on the spec.
func podLabels(treebind *treebindv1alpha1.Treebind) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "treebind",
		"app.kubernetes.io/instance":  treebind.Name,
		"app.kubernetes.io/component": "sync-agent",
	}
}

func objectLabels(treebind *treebindv1alpha1.Treebind) map[string]string {
	labels := podLabels(treebind)
	labels["app.kubernetes.io/managed-by"] = "treebind-operator"
	return labels
}

// desiredService returns the headless Service that gives the sync pod its
// stable identity. The owner reference is added by the caller.
func desiredService(treebind *treebindv1alpha1.Treebind) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncName(treebind),
			Namespace: treebind.Namespace,
			Labels:    objectLabels(treebind),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  podLabels(treebind),
		},
	}
}

// desiredStatefulSet returns the StatefulSet that runs the single sync pod of
// a Treebind, with the hash of its pod template in templateHashAnnotation. The
// owner reference is added by the caller.
func desiredStatefulSet(treebind *treebindv1alpha1.Treebind, agentImage string) (*appsv1.StatefulSet, error) {
	template := desiredPodTemplate(treebind, agentImage)
	hash, err := templateHash(template)
	if err != nil {
		return nil, err
	}

	replicas := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        syncName(treebind),
			Namespace:   treebind.Namespace,
			Labels:      objectLabels(treebind),
			Annotations: map[string]string{templateHashAnnotation: hash},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: podLabels(treebind)},
			ServiceName: syncName(treebind),
			// Parallel, not OrderedReady: with one pod there is no order to keep,
			// and OrderedReady stops the StatefulSet controller from replacing a
			// pod that never became Ready, so correcting a wrong Secret name in
			// the spec would never roll the stuck pod. The pod name is still held
			// by the old pod until it is gone, so two pods never overlap.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			Template:            template,
		},
	}, nil
}

func desiredPodTemplate(treebind *treebindv1alpha1.Treebind, agentImage string) corev1.PodTemplateSpec {
	mountPath := treebind.Spec.Target.MountPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}

	volumes := []corev1.Volume{{
		Name: dataVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: treebind.Spec.Target.ClaimName,
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: dataVolumeName, MountPath: mountPath}}

	if auth := treebind.Spec.Repository.Auth; auth != nil {
		if ref := auth.SSHKeySecretRef; ref != nil {
			mode := sshKeyFileMode
			volumes = append(volumes, corev1.Volume{
				Name: sshVolumeName,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  ref.Name,
					Items:       []corev1.KeyToPath{{Key: ref.Key, Path: ref.Key}},
					DefaultMode: &mode,
				}},
			})
			mounts = append(mounts, corev1.VolumeMount{Name: sshVolumeName, MountPath: sshMountPath, ReadOnly: true})
		}
		if ref := auth.KnownHostsConfigMapRef; ref != nil {
			volumes = append(volumes, corev1.Volume{
				Name: knownHostsVolumeName,
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
					Items:                []corev1.KeyToPath{{Key: ref.Key, Path: ref.Key}},
				}},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name: knownHostsVolumeName, MountPath: knownHostsMountPath, ReadOnly: true,
			})
		}
	}

	runAsNonRoot, noEscalation, readOnlyRoot := true, false, true
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(treebind)},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &runAsNonRoot,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  agentContainerName,
				Image: agentImage,
				Env: []corev1.EnvVar{
					{Name: "TREEBIND_NAME", Value: treebind.Name},
					{Name: "TREEBIND_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
					}},
				},
				VolumeMounts: mounts,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: volumes,
		},
	}
}

// templateHash returns the SHA-256 of the JSON encoding of the pod template.
// The API server fills defaults into the template of the stored StatefulSet,
// so comparing the stored template with the desired one would always differ.
// Comparing hashes of what the operator wrote does not.
func templateHash(template corev1.PodTemplateSpec) (string, error) {
	encoded, err := json.Marshal(template)
	if err != nil {
		return "", fmt.Errorf("encode pod template: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ensureService creates the headless Service or brings it back to the desired
// state. It writes only when something differs.
func (r *TreebindReconciler) ensureService(ctx context.Context, treebind *treebindv1alpha1.Treebind) error {
	desired := desiredService(treebind)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = mergeMap(service.Labels, desired.Labels)
		service.Spec.Selector = desired.Spec.Selector
		// The cluster IP cannot change after creation.
		if service.CreationTimestamp.IsZero() {
			service.Spec.ClusterIP = desired.Spec.ClusterIP
		}
		return controllerutil.SetControllerReference(treebind, service, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure Service %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	logResult(ctx, "Service", client.ObjectKeyFromObject(service).String(), result)
	return nil
}

// ensureStatefulSet creates the sync StatefulSet or brings it back to the
// desired state, and returns it as the API server last reported it. It always
// restores one replica, rewrites the pod template only when its hash changed,
// and sets the fields a StatefulSet cannot change only on creation, so a
// reconcile with nothing to do writes nothing.
func (r *TreebindReconciler) ensureStatefulSet(
	ctx context.Context, treebind *treebindv1alpha1.Treebind,
) (*appsv1.StatefulSet, error) {
	desired, err := desiredStatefulSet(treebind, r.AgentImage)
	if err != nil {
		return nil, fmt.Errorf("build StatefulSet for Treebind %s/%s: %w", treebind.Namespace, treebind.Name, err)
	}
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, statefulSet, func() error {
		statefulSet.Labels = mergeMap(statefulSet.Labels, desired.Labels)
		if statefulSet.CreationTimestamp.IsZero() {
			statefulSet.Spec.Selector = desired.Spec.Selector
			statefulSet.Spec.ServiceName = desired.Spec.ServiceName
			statefulSet.Spec.PodManagementPolicy = desired.Spec.PodManagementPolicy
			statefulSet.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
		}
		statefulSet.Spec.Replicas = desired.Spec.Replicas

		if statefulSet.Annotations[templateHashAnnotation] != desired.Annotations[templateHashAnnotation] {
			statefulSet.Spec.Template = desired.Spec.Template
			statefulSet.Annotations = mergeMap(statefulSet.Annotations, desired.Annotations)
		}
		return controllerutil.SetControllerReference(treebind, statefulSet, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure StatefulSet %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	logResult(ctx, "StatefulSet", client.ObjectKeyFromObject(statefulSet).String(), result)
	return statefulSet, nil
}

func mergeMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

func logResult(ctx context.Context, kind, name string, result controllerutil.OperationResult) {
	if result == controllerutil.OperationResultNone {
		return
	}
	logf.FromContext(ctx).Info("Applied sync object", "kind", kind, "name", name, "result", result)
}
