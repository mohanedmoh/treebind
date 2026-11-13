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
	"errors"
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
	// StatusIssuer is the issuer of the cluster's service account tokens, and
	// StatusSubject the operator's own service account, as the sub claim names it
	// (system:serviceaccount:<namespace>:<name>). The agent accepts only a token
	// that carries them (docs/adr/0011).
	StatusIssuer, StatusSubject string
	// StatusKeys puts the cluster's public signing keys where the agents read them.
	StatusKeys *StatusKeyPublisher
}

// agentSettings is what the sync pods are given beside the spec of a Treebind.
func (r *TreebindReconciler) agentSettings() agentSettings {
	return agentSettings{Image: r.AgentImage, StatusIssuer: r.StatusIssuer, StatusSubject: r.StatusSubject}
}

// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// awaitingReportMessage is the message of the Synced and Conflict conditions
// until the sync agent's first report arrives.
const awaitingReportMessage = "No report from the sync agent has been received yet"

// Reconcile keeps the headless Service, the one-replica StatefulSet and the
// ConfigMap of keys that run the sync agent of a Treebind, and reports in the
// Ready condition whether the sync pod is ready and the volume is not known to be
// out of sync. It also reports in the SpecSupported condition which fields of the
// spec are accepted but not implemented yet. It adds the Synced and Conflict
// conditions as Unknown the first time it sees a Treebind and never changes them
// afterwards: the status poller owns them. It writes the status only when a
// condition or the observed generation changed, so the status update it makes does
// not trigger a reconcile that writes again.
//
// When it cannot create or update the sync objects it still writes the status,
// with Ready False and the reason ReconcileFailed, and then returns the error so
// that the request is retried. An error that a retry makes go away is the
// exception, because reporting it would only make Ready flap.
//
// It puts the cluster's public signing keys in a ConfigMap for the sync agent
// before it creates the StatefulSet, so that a new pod finds the file when the
// kubelet mounts the volume instead of waiting for it to appear. A failure to do
// that does not change Ready and does not stop the sync objects: the sync does not
// need the keys, only the operator's reading of the agent does, and Ready is about
// the sync. The failure is logged and the pass is asked for again after about
// statusKeysRetry. A pass that succeeds is asked for again after about
// statusKeysRefresh, because the keys can change with nothing in the cluster to
// say so.
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

	keysErr := r.StatusKeys.Ensure(ctx, r.Client, r.Scheme, treebind)
	if keysErr != nil && ctx.Err() == nil {
		if errors.Is(keysErr, errReadKeys) {
			// The key source has logged it, once for every Treebind.
			log.V(1).Info("Could not read the cluster's public signing keys", "error", keysErr.Error())
		} else {
			log.Error(keysErr, "Could not put the cluster's public signing keys where the sync agent reads them")
		}
	}
	statefulSet, ensureErr := r.ensureSyncObjects(ctx, treebind)
	if ensureErr != nil && resolvesOnRetry(ctx, ensureErr) {
		return ctrl.Result{}, ensureErr
	}

	changed = addAwaitingReportConditions(treebind) || changed
	var ready metav1.Condition
	if ensureErr != nil {
		ready = reconcileFailedCondition(ensureErr, treebind.Generation)
	} else {
		synced := meta.FindStatusCondition(treebind.Status.Conditions, treebindv1alpha1.ConditionSynced)
		ready = readyCondition(statefulSet, synced, treebind.Generation)
	}
	changed = meta.SetStatusCondition(&treebind.Status.Conditions, ready) || changed

	if treebind.Status.ObservedGeneration != treebind.Generation {
		treebind.Status.ObservedGeneration = treebind.Generation
		changed = true
	}
	if changed {
		// A conflict means the object changed after it was read. The error requeues
		// the request, and the next pass starts from the newer object.
		if err := r.Status().Update(ctx, treebind); err != nil {
			return ctrl.Result{}, errors.Join(ensureErr,
				fmt.Errorf("update status of Treebind %s: %w", req.NamespacedName, err))
		}
		log.Info("Reported status", "supported", len(fields) == 0, "unimplementedFields", fields,
			"ready", ready.Status, "reason", ready.Reason)
	}

	if ensureErr != nil {
		return ctrl.Result{}, ensureErr
	}
	if keysErr != nil {
		return ctrl.Result{RequeueAfter: jittered(statusKeysRetry)}, nil
	}
	return ctrl.Result{RequeueAfter: jittered(statusKeysRefresh)}, nil
}

// ensureSyncObjects creates or updates the Service and then the StatefulSet. It
// returns the StatefulSet as the API server last reported it, or the error of the
// step that failed.
func (r *TreebindReconciler) ensureSyncObjects(
	ctx context.Context, treebind *treebindv1alpha1.Treebind,
) (*appsv1.StatefulSet, error) {
	if err := r.ensureService(ctx, treebind); err != nil {
		return nil, err
	}
	return r.ensureStatefulSet(ctx, treebind)
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

// addAwaitingReportConditions adds the Synced and Conflict conditions as Unknown
// when they are absent, and reports whether it added one. A condition that is
// there is never touched, whatever its status, because the status poller wrote
// it.
func addAwaitingReportConditions(treebind *treebindv1alpha1.Treebind) bool {
	added := false
	for _, conditionType := range []string{treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict} {
		if meta.FindStatusCondition(treebind.Status.Conditions, conditionType) != nil {
			continue
		}
		meta.SetStatusCondition(&treebind.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             metav1.ConditionUnknown,
			Reason:             treebindv1alpha1.ReasonAwaitingAgentReport,
			Message:            awaitingReportMessage,
			ObservedGeneration: treebind.Generation,
		})
		added = true
	}
	return added
}

// resolvesOnRetry reports whether a retry makes the error go away, so that
// reporting it would only turn Ready False for a moment. An update conflict and an
// object that already exists come from a cache that has not caught up with the
// write the previous pass made, and a cancelled context means the manager is
// stopping.
func resolvesOnRetry(ctx context.Context, err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || ctx.Err() != nil
}

// maxFailureText is the most bytes of an error that go into the Ready message.
// The API server can answer with a long list of field errors, and the message
// stays on the object.
const maxFailureText = 1024

// reconcileFailedCondition builds the Ready condition for a pass that could not
// create or update the sync objects. The error is the API server's answer about
// objects built from the spec, and the operator never reads a Secret, so it
// holds no credential.
func reconcileFailedCondition(err error, generation int64) metav1.Condition {
	text := err.Error()
	if len(text) > maxFailureText {
		text = strings.ToValidUTF8(text[:maxFailureText], "") + "..."
	}
	return metav1.Condition{
		Type:               treebindv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             treebindv1alpha1.ReasonReconcileFailed,
		Message:            "The last reconcile failed: " + text,
		ObservedGeneration: generation,
	}
}

// readyCondition builds the Ready condition from the StatefulSet status and the
// Synced condition, which is nil while it is absent. A ready count is only
// trusted once the StatefulSet controller has observed the current generation,
// because before that it describes the previous template. A ready pod makes
// Ready False only when Synced is False; Unknown means no report yet and does
// not. Conflict is not an input: with followRemote the sync keeps running while
// a conflict waits for attention.
func readyCondition(statefulSet *appsv1.StatefulSet, synced *metav1.Condition, generation int64) metav1.Condition {
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
	if ready < 1 {
		return condition
	}
	if synced != nil && synced.Status == metav1.ConditionFalse {
		condition.Reason = treebindv1alpha1.ReasonNotSynced
		condition.Message = fmt.Sprintf("%d of 1 sync pods ready, but Synced is False: %s", ready, synced.Reason)
		return condition
	}
	condition.Status = metav1.ConditionTrue
	condition.Reason = treebindv1alpha1.ReasonSyncPodReady
	return condition
}

// SetupWithManager sets up the controller with the Manager.
func (r *TreebindReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// An agent that is given no issuer or no subject exits at once and is started
	// again for ever, so the operator refuses to start instead. The pod template
	// refuses them too, for a reconciler that was not set up here.
	if err := r.agentSettings().validate(); err != nil {
		return err
	}
	if r.StatusKeys == nil || r.StatusKeys.Source == nil {
		return errors.New("a source of the cluster's public signing keys is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&treebindv1alpha1.Treebind{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("treebind").
		Complete(r)
}
