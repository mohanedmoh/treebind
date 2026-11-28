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
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// imageUserID is the user that the agent image runs as, the one user that the image's /etc/passwd has an entry for.
const imageUserID int64 = 65532

// defaultAgentResources are the resources of the sync container that spec.agent.resources does not set.
func defaultAgentResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

// agentResources returns the resources of the sync container: the defaults, with each request and each limit that set
// has in the place of the default of its resource (docs/adr/0044). A default never makes the result invalid next to
// what was set: a default limit below a request that was set is raised to the request, and a default request above a
// limit that was set is lowered to the limit, which is what the API server does to a request that is not set. A request
// above a limit that were both set is left as it is, for the API server to refuse with its own words. The claims have
// no counterpart in the pod and are left out; SpecSupported reports them.
func agentResources(set *corev1.ResourceRequirements) corev1.ResourceRequirements {
	resources := defaultAgentResources()
	if set == nil {
		return resources
	}
	for name, quantity := range set.Requests {
		resources.Requests[name] = quantity.DeepCopy()
	}
	for name, quantity := range set.Limits {
		resources.Limits[name] = quantity.DeepCopy()
	}
	for name, request := range resources.Requests {
		limit, limited := resources.Limits[name]
		if !limited || request.Cmp(limit) <= 0 {
			continue
		}
		_, requestSet := set.Requests[name]
		_, limitSet := set.Limits[name]
		switch {
		case requestSet && !limitSet:
			resources.Limits[name] = request.DeepCopy()
		case limitSet && !requestSet:
			resources.Requests[name] = limit.DeepCopy()
		}
	}
	return resources
}

// runsAsRoot says whether spec.agent asks for the sync pod to run as root. The sync pod keeps runAsNonRoot, so the
// kubelet would refuse to start it, and the operator makes no pod of it (docs/adr/0044).
func runsAsRoot(agent *treebindv1alpha1.AgentSpec) bool {
	return agent != nil && agent.RunAsUser != nil && *agent.RunAsUser == 0
}

// needsPasswdFile says whether spec.agent runs the sync pod as a user that the image's passwd file has no entry for,
// which ssh refuses to run for, so that the pod is given one whatever the operator's setting (docs/adr/0042, 0044).
func needsPasswdFile(agent *treebindv1alpha1.AgentSpec) bool {
	return agent != nil && agent.RunAsUser != nil && *agent.RunAsUser != imageUserID
}

// withAgentPodSpec applies to the template what spec.agent says of the pod: the resources of the sync container, where the
// pod may run, its priority, the Secrets to pull the image with, and its user and group. It adds to the pod security
// context and never takes from it: runAsNonRoot, the seccomp profile and the security context of the container stay. A
// field that is not set, or a collection that is empty, changes nothing, so that the template of a Treebind without
// spec.agent is the one it was. fsGroup is not applied: the agent refuses its own directory after the kubelet has
// walked the claim for one (docs/adr/0044).
func withAgentPodSpec(template *corev1.PodTemplateSpec, agent *treebindv1alpha1.AgentSpec) {
	if agent == nil {
		return
	}
	spec := &template.Spec
	if resources := agent.Resources; resources != nil && (len(resources.Requests) > 0 || len(resources.Limits) > 0) {
		spec.Containers[0].Resources = agentResources(resources)
	}
	if len(agent.NodeSelector) > 0 {
		spec.NodeSelector = maps.Clone(agent.NodeSelector)
	}
	if len(agent.Tolerations) > 0 {
		spec.Tolerations = slices.Clone(agent.Tolerations)
	}
	spec.PriorityClassName = agent.PriorityClassName
	if len(agent.ImagePullSecrets) > 0 {
		spec.ImagePullSecrets = slices.Clone(agent.ImagePullSecrets)
	}
	if agent.RunAsUser != nil {
		user := *agent.RunAsUser
		spec.SecurityContext.RunAsUser = &user
	}
	if agent.RunAsGroup != nil {
		group := *agent.RunAsGroup
		spec.SecurityContext.RunAsGroup = &group
	}
}

// withAgentMetadata adds the labels and the annotations of spec.agent to the template, after the operator has put its
// own there: a key that the operator sets keeps the operator's value, so that the selector of the StatefulSet still
// selects the pod, and a key that it does not set has the value of spec.agent.
func withAgentMetadata(meta *metav1.ObjectMeta, agent *treebindv1alpha1.AgentSpec) {
	if agent == nil {
		return
	}
	meta.Labels = withoutOverriding(meta.Labels, agent.Labels)
	meta.Annotations = withoutOverriding(meta.Annotations, agent.Annotations)
}

// withoutOverriding adds to dst each entry of src whose key dst does not have. It makes dst only when there is
// something to add, so that a template without annotations stays without them.
func withoutOverriding(dst, src map[string]string) map[string]string {
	for key, value := range src {
		if _, set := dst[key]; set {
			continue
		}
		if dst == nil {
			dst = make(map[string]string, len(src))
		}
		dst[key] = value
	}
	return dst
}
