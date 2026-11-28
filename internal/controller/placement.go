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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// Where the sync pod runs (FR-25, docs/adr/0046). It is the scheduler that chooses the node: the operator never sets
// nodeName, which would go past the scheduler and its checks of the volume, the taints and the room on the node.

// claimAccessModes returns the access modes that the claim of a Treebind with an appSelector asks for, and none for a
// claim that is not there. A Treebind without an appSelector needs no answer, and its claim is not read.
//
// The claim is read from the API server and not from the cache, so that the operator needs only get on claims and no
// watch of every claim (docs/rbac.md). The access modes of a claim cannot be changed, so a pass later than the change
// only matters for a claim that was made after its Treebind, or deleted and made again: the sync pod is not ready then,
// and such a pass is asked for again after about syncPodRecheck.
func (r *TreebindReconciler) claimAccessModes(
	ctx context.Context, treebind *treebindv1alpha1.Treebind,
) ([]corev1.PersistentVolumeAccessMode, error) {
	if treebind.Spec.Placement.AppSelector == nil || r.APIReader == nil {
		return nil, nil
	}
	key := client.ObjectKey{Namespace: treebind.Namespace, Name: treebind.Spec.Target.ClaimName}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.APIReader.Get(ctx, key, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get PersistentVolumeClaim %s: %w", key, err)
	}
	return claim.Spec.AccessModes, nil
}

// appAffinity returns the affinity that puts the sync pod on the node of a pod that selector selects, or nil when the
// sync pod needs none: without a selector, and for a claim with these access modes that more than one node can mount.
// No access modes means that they are not known, as for a claim that is not there, and then the affinity is set: a sync
// pod that waits can be placed later, while one that took a volume to another node than the application's keeps the
// application from starting.
func appAffinity(selector *metav1.LabelSelector, claimModes []corev1.PersistentVolumeAccessMode) *corev1.Affinity {
	if selector == nil || manyNodesCanMount(claimModes) {
		return nil
	}
	// A sync pod is not the application. Without this, a selector that matches the sync pod, by the labels of the
	// operator or of spec.agent, would be satisfied by the pod itself: Kubernetes places the first pod of a group that
	// has affinity to itself anywhere.
	beside := selector.DeepCopy()
	beside.MatchExpressions = append(beside.MatchExpressions, metav1.LabelSelectorRequirement{
		Key: managedByLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{managedByOperator},
	})
	// With no namespaces and no namespaceSelector the term is of the namespace of the sync pod, which is the
	// Treebind's. IgnoredDuringExecution is the only kind there is: a sync pod stays where it is when the application
	// moves (#113).
	return &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: beside,
			TopologyKey:   corev1.LabelHostname,
		}},
	}}
}

// manyNodesCanMount says whether a claim with these access modes can be mounted by the sync pod on another node than
// the application's: it lists ReadWriteMany, or it lists neither ReadWriteOnce nor ReadWriteOncePod, which leaves
// ReadOnlyMany. No access modes says nothing, and is false.
//
// ReadWriteOncePod is one pod and not only one node, so no affinity lets the two share such a claim. The affinity is
// set all the same: the sync pod is then never placed before the application is, so it cannot take the claim first and
// leave the application Pending.
func manyNodesCanMount(claimModes []corev1.PersistentVolumeAccessMode) bool {
	if slices.Contains(claimModes, corev1.ReadWriteMany) {
		return true
	}
	return len(claimModes) > 0 && !slices.Contains(claimModes, corev1.ReadWriteOnce) &&
		!slices.Contains(claimModes, corev1.ReadWriteOncePod)
}

// besideTheApplicationOnly says whether the pod template of the StatefulSet has a required pod affinity, which only
// appAffinity gives it.
func besideTheApplicationOnly(affinity *corev1.Affinity) bool {
	return affinity != nil && affinity.PodAffinity != nil &&
		len(affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}
