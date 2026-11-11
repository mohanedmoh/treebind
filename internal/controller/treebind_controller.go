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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// TreebindReconciler reconciles a Treebind object
type TreebindReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AgentImage is the image of the sync container.
	AgentImage string
}

// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile keeps the headless Service and the one-replica StatefulSet that
// run the sync agent of a Treebind, and reports in the Ready condition whether
// the sync pod is ready. It also reports in the SpecSupported condition which
// fields of the spec are accepted but not implemented yet. It writes the
// status only when a condition or the observed generation changed, so the
// status update it makes does not trigger a reconcile that writes again.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *TreebindReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	treebind := &treebindv1alpha1.Treebind{}
	if err := r.Get(ctx, req.NamespacedName, treebind); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Treebind %s: %w", req.NamespacedName, err)
	}

	// Foreground deletion keeps the Treebind while the garbage collector removes
	// the sync objects. Recreating them here would fight the collector.
	if !treebind.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	fields := unimplementedFields(&treebind.Spec)
	changed := meta.SetStatusCondition(&treebind.Status.Conditions, specSupportedCondition(fields, treebind.Generation))

	if err := r.ensureService(ctx, treebind); err != nil {
		return ctrl.Result{}, err
	}
	statefulSet, err := r.ensureStatefulSet(ctx, treebind)
	if err != nil {
		return ctrl.Result{}, err
	}
	changed = meta.SetStatusCondition(&treebind.Status.Conditions, readyCondition(statefulSet, treebind.Generation)) ||
		changed

	if treebind.Status.ObservedGeneration != treebind.Generation {
		treebind.Status.ObservedGeneration = treebind.Generation
		changed = true
	}
	if !changed {
		return ctrl.Result{}, nil
	}

	// A conflict means the object changed after it was read. The error requeues
	// the request, and the next pass starts from the newer object.
	if err := r.Status().Update(ctx, treebind); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status of Treebind %s: %w", req.NamespacedName, err)
	}
	log.Info("Reported spec support", "supported", len(fields) == 0, "unimplementedFields", fields)

	return ctrl.Result{}, nil
}

// unimplementedFields returns the dotted JSON paths of the spec fields that are
// set but have no effect yet, sorted. A pointer counts as set once it points at
// a value, because runAsUser: 0 is a choice, and a collection counts as set
// once it has an entry.
func unimplementedFields(spec *treebindv1alpha1.TreebindSpec) []string {
	var fields []string
	if spec.Placement.Mode == treebindv1alpha1.PlacementModeSidecar {
		fields = append(fields, "spec.placement.mode")
	}

	if agent := spec.Agent; agent != nil {
		resources := agent.Resources
		for _, field := range []struct {
			path string
			set  bool
		}{
			{"spec.agent.resources", resources != nil &&
				(len(resources.Limits) > 0 || len(resources.Requests) > 0 || len(resources.Claims) > 0)},
			{"spec.agent.nodeSelector", len(agent.NodeSelector) > 0},
			{"spec.agent.tolerations", len(agent.Tolerations) > 0},
			{"spec.agent.priorityClassName", agent.PriorityClassName != ""},
			{"spec.agent.imagePullSecrets", len(agent.ImagePullSecrets) > 0},
			{"spec.agent.labels", len(agent.Labels) > 0},
			{"spec.agent.annotations", len(agent.Annotations) > 0},
			{"spec.agent.runAsUser", agent.RunAsUser != nil},
			{"spec.agent.runAsGroup", agent.RunAsGroup != nil},
			{"spec.agent.fsGroup", agent.FSGroup != nil},
		} {
			if field.set {
				fields = append(fields, field.path)
			}
		}
	}

	slices.Sort(fields)
	return fields
}

// specSupportedCondition builds the SpecSupported condition for the given
// unimplemented fields. LastTransitionTime is left for meta.SetStatusCondition,
// which sets it only when the status flips.
func specSupportedCondition(fields []string, generation int64) metav1.Condition {
	if len(fields) == 0 {
		return metav1.Condition{
			Type:               treebindv1alpha1.ConditionSpecSupported,
			Status:             metav1.ConditionTrue,
			Reason:             treebindv1alpha1.ReasonSupported,
			Message:            "Every field set in the spec is implemented",
			ObservedGeneration: generation,
		}
	}
	return metav1.Condition{
		Type:               treebindv1alpha1.ConditionSpecSupported,
		Status:             metav1.ConditionFalse,
		Reason:             treebindv1alpha1.ReasonNotImplemented,
		Message:            "Accepted but ignored until implemented: " + strings.Join(fields, ", "),
		ObservedGeneration: generation,
	}
}

// readyCondition builds the Ready condition from the StatefulSet status. A
// ready count is only trusted once the StatefulSet controller has observed the
// current generation, because before that it describes the previous template.
func readyCondition(statefulSet *appsv1.StatefulSet, generation int64) metav1.Condition {
	var ready int32
	if statefulSet.Status.ObservedGeneration >= statefulSet.Generation {
		ready = statefulSet.Status.ReadyReplicas
	}

	condition := metav1.Condition{
		Type:               treebindv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             treebindv1alpha1.ReasonSyncPodNotReady,
		Message:            fmt.Sprintf("%d of 1 sync pods ready", ready),
		ObservedGeneration: generation,
	}
	if ready >= 1 {
		condition.Status = metav1.ConditionTrue
		condition.Reason = treebindv1alpha1.ReasonSyncPodReady
	}
	return condition
}

// SetupWithManager sets up the controller with the Manager.
func (r *TreebindReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&treebindv1alpha1.Treebind{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("treebind").
		Complete(r)
}
