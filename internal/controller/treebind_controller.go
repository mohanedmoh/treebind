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
	"time"

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
	// AgentArbitraryUserID gives every sync pod the passwd file of the user that it runs
	// as, so that it runs under any user ID (docs/adr/0042).
	AgentArbitraryUserID bool
	// StatusIssuer is the issuer of the cluster's service account tokens, and
	// StatusSubject the operator's own service account, as the sub claim names it
	// (system:serviceaccount:<namespace>:<name>). The agent accepts only a token
	// that carries them (docs/adr/0011).
	StatusIssuer, StatusSubject string
	// StatusKeys puts the cluster's public signing keys where the agents read them.
	StatusKeys *StatusKeyPublisher
	// Metrics holds the metrics of the Treebinds, which the status poller shares. The reconcile records in it the
	// conditions that it owns, Ready and SpecSupported (metrics.go).
	Metrics *Metrics
	// APIReader reads the sync pod from the API server and not from the cache, so that the operator needs only get on
	// pods and not a watch of every pod (docs/rbac.md). The reconcile reads the pod only while it is not ready, to say
	// whether the scheduler cannot place it, its node is not ready or its container was killed for memory.
	// SetupWithManager requires it, and a reconciler without one, as a test makes, reads no pod.
	APIReader client.Reader

	// now is time.Now unless a test replaces it.
	now func() time.Time
}

// agentSettings is what the sync pods are given beside the spec of a Treebind.
func (r *TreebindReconciler) agentSettings() agentSettings {
	return agentSettings{
		Image: r.AgentImage, StatusIssuer: r.StatusIssuer, StatusSubject: r.StatusSubject,
		ArbitraryUserID: r.AgentArbitraryUserID,
	}
}

func (r *TreebindReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// What the operator may do is what its code does and no more (NFR-5): it reads Treebinds and writes their
// status, and it creates and updates the StatefulSet and the Service that a Treebind owns, which the garbage
// collector deletes with it. The finalizers verb is for the owner reference of what it creates, which sets
// blockOwnerDeletion. It gets the sync pod by its name, and lists and watches no pod, to say why the pod is not ready.
// The ConfigMap of the keys is in statuskeys.go. rbac_exact_test.go holds each list.
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds,verbs=get;list;watch
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds/status,verbs=update
// +kubebuilder:rbac:groups=treebind.example.org,resources=treebinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get

// awaitingReportMessage is the message of the Synced and Conflict conditions
// until the sync agent's first report arrives.
const awaitingReportMessage = "No report from the sync agent has been received yet"

// Reconcile keeps the headless Service, the one-replica StatefulSet and the
// ConfigMap of keys that run the sync agent of a Treebind, and reports in the
// Ready condition whether the sync pod is ready and the volume is not known to be
// out of sync. It also reports in the SpecSupported condition which fields of the
// spec are accepted but not implemented yet. It adds the Synced, Conflict and
// AgentUnreachable conditions as Unknown the first time it sees a Treebind and
// never changes them afterwards: the status poller owns them. It writes the status only when a
// condition or the observed generation changed, so the status update it makes does
// not trigger a reconcile that writes again.
//
// A Treebind whose repository address has the scheme http gets no sync objects, because the agent refuses it at
// start: Ready is False with the reason UnsupportedRepositoryScheme, and no error is returned, since only a change of
// the spec puts it right.
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
// say so, and after about syncPodRecheck while no sync pod is ready.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *TreebindReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	treebind := &treebindv1alpha1.Treebind{}
	if err := r.Get(ctx, req.NamespacedName, treebind); err != nil {
		if apierrors.IsNotFound(err) {
			r.Metrics.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Treebind %s: %w", req.NamespacedName, err)
	}

	// Foreground deletion keeps the Treebind while the garbage collector removes
	// the sync objects. Recreating them here would fight the collector.
	if !treebind.DeletionTimestamp.IsZero() {
		r.Metrics.forget(req.NamespacedName)
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
	// The agent refuses an http:// address at start, so a sync pod made for one would only crash (repository_scheme.go).
	plainHTTP := usesPlainHTTP(treebind.Spec.Repository.URL)
	var statefulSet *appsv1.StatefulSet
	var ensureErr error
	if !plainHTTP {
		statefulSet, ensureErr = r.ensureSyncObjects(ctx, treebind)
		if ensureErr != nil && resolvesOnRetry(ctx, ensureErr) {
			return ctrl.Result{}, ensureErr
		}
	}

	changed = addAwaitingReportConditions(treebind) || changed
	var ready metav1.Condition
	switch {
	case plainHTTP:
		ready = unsupportedRepositorySchemeCondition(treebind.Generation)
	case ensureErr != nil:
		ready = reconcileFailedCondition(ensureErr, treebind.Generation)
	default:
		synced := meta.FindStatusCondition(treebind.Status.Conditions, treebindv1alpha1.ConditionSynced)
		ready = readyCondition(statefulSet, synced, treebind.Spec.Sync.Direction, treebind.Generation)
		if ready.Reason == treebindv1alpha1.ReasonSyncPodNotReady {
			ready = r.whySyncPodNotReady(ctx, statefulSet, ready)
		}
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
	r.Metrics.recordConditions(treebind, treebindv1alpha1.ConditionReady, treebindv1alpha1.ConditionSpecSupported)

	if ensureErr != nil {
		return ctrl.Result{}, ensureErr
	}
	after := jittered(statusKeysRefresh)
	if keysErr != nil {
		after = jittered(statusKeysRetry)
	}
	// Without a reader of the pod a pass sooner learns nothing new.
	if r.APIReader != nil && slices.Contains(syncPodNotReadyReasons, ready.Reason) {
		after = min(after, jittered(syncPodRecheck))
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// syncPodRecheck is about how soon a pass is asked for again while no sync pod is ready. A pod that has never been ready
// gives its StatefulSet nothing to change while its container is killed and started again, or while it waits for the
// scheduler, so no event starts a pass that reads the pod, and Ready would say why only after the refresh of the keys
// (#256, #266).
const syncPodRecheck = 30 * time.Second

// syncPodNotReadyReasons are the reasons of Ready while no sync pod is ready: the one of readyCondition, and the ones
// that whySyncPodNotReady gives once it has read the pod.
var syncPodNotReadyReasons = []string{
	treebindv1alpha1.ReasonSyncPodNotReady, treebindv1alpha1.ReasonSyncPodUnschedulable,
	treebindv1alpha1.ReasonSyncPodNodeNotReady, treebindv1alpha1.ReasonSyncPodOOMKilled,
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

// addAwaitingReportConditions adds the conditions that the status poller owns, as
// Unknown, when they are absent, and reports whether it added one. A condition
// that is there is never touched, whatever its status, because the status poller
// wrote it.
func addAwaitingReportConditions(treebind *treebindv1alpha1.Treebind) bool {
	added := false
	for _, conditionType := range []string{
		treebindv1alpha1.ConditionSynced,
		treebindv1alpha1.ConditionConflict,
		treebindv1alpha1.ConditionAgentUnreachable,
	} {
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

// readyCondition builds the Ready condition from the StatefulSet status, the Synced condition, which is nil while it
// is absent, and the sync direction and the generation of the Treebind. A ready count is only trusted once the
// StatefulSet controller has observed the current generation, because before that it describes the previous template.
//
// A ready pod makes Ready False, with NotSynced, only when Synced says that a step of the sync failed, of the current
// spec, for a reason that waits for a person (docs/adr/0004, docs/adr/0015, docs/adr/0030):
//   - Unknown means no report yet, or none that says anything of this spec, and does not.
//   - A Synced of an older generation is the report of the spec before, which a spec change leaves in place until the
//     status poller writes a new one at its next poll, so it counts as Unknown and does not either.
//   - A reason that waits for the repository does not, whatever the direction makes of it (WaitsForRepository). Ready is
//     what kubectl wait and the health checks of a GitOps controller act on, and a Git host that is down would turn it
//     False for every Treebind that uses the host for as long as it is down, a reason that is not the data path.
//     Synced says it, with the reason, and the sync tries again by itself.
//
// Conflict is not an input: with followRemote the sync keeps running while a conflict waits for attention.
func readyCondition(
	statefulSet *appsv1.StatefulSet, synced *metav1.Condition, direction treebindv1alpha1.SyncDirection, generation int64,
) metav1.Condition {
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
	if syncFailureWaitsForPerson(synced, direction, generation) {
		condition.Reason = treebindv1alpha1.ReasonNotSynced
		condition.Message = fmt.Sprintf("%d of 1 sync pods ready, but Synced is False: %s", ready, synced.Reason)
		return condition
	}
	condition.Status = metav1.ConditionTrue
	condition.Reason = treebindv1alpha1.ReasonSyncPodReady
	return condition
}

// syncFailureWaitsForPerson says whether Synced reports, of the Treebind's current generation, a failed step whose
// reason waits for a person, which is what makes a ready pod not ready.
func syncFailureWaitsForPerson(
	synced *metav1.Condition, direction treebindv1alpha1.SyncDirection, generation int64,
) bool {
	return synced != nil &&
		synced.Status == metav1.ConditionFalse &&
		synced.ObservedGeneration >= generation &&
		!WaitsForRepository(synced.Reason, direction)
}

// whySyncPodNotReady reads the one pod of the StatefulSet and returns the Ready condition of a sync pod that is not
// ready with what the pod says of the cause: the scheduler cannot place it (unschedulableCondition), its node is not
// ready (nodeNotReadyCondition), or its container was killed for memory (oomKilledCondition). A pod that is not there
// yet, or that cannot be read, leaves the condition as it is: the status says less, and the sync is not touched.
func (r *TreebindReconciler) whySyncPodNotReady(
	ctx context.Context, statefulSet *appsv1.StatefulSet, ready metav1.Condition,
) metav1.Condition {
	if r.APIReader == nil {
		return ready
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: statefulSet.Namespace, Name: statefulSet.Name + "-0"}
	if err := r.APIReader.Get(ctx, key, pod); err != nil {
		if !apierrors.IsNotFound(err) && ctx.Err() == nil {
			logf.FromContext(ctx).Error(err, "Could not read the sync pod to say why it is not ready", "pod", key.Name)
		}
		return ready
	}
	if scheduling := unschedulable(pod); scheduling != nil {
		return unschedulableCondition(ready, pod.Name, scheduling.Message)
	}
	if onNodeThatIsNotReady(pod) {
		return nodeNotReadyCondition(ready, pod.Name, pod.Spec.NodeName)
	}
	return oomKilledCondition(ready, pod, r.clock())
}

// podCondition returns the condition of the pod of the type, and nil when the pod has none.
func podCondition(pod *corev1.Pod, conditionType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == conditionType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// unschedulable returns the PodScheduled condition of a pod that is Pending because the scheduler cannot place it, and
// nil for any other pod. Its message is the scheduler's, which names what no node has: for a claim whose volume only
// one node can mount, a volume node affinity conflict and the node that was cordoned or drained (#266).
func unschedulable(pod *corev1.Pod) *corev1.PodCondition {
	scheduled := podCondition(pod, corev1.PodScheduled)
	if pod.Status.Phase != corev1.PodPending || scheduled == nil || scheduled.Status != corev1.ConditionFalse ||
		scheduled.Reason != corev1.PodReasonUnschedulable {
		return nil
	}
	return scheduled
}

// unschedulableCondition returns the Ready condition of a sync pod that is Pending because the scheduler cannot place it,
// with the reason SyncPodUnschedulable and the scheduler's words, cut as the error of a failed reconcile is. While it
// is Pending the pod has no address, so the operator cannot read the agent, and nothing is synced.
func unschedulableCondition(ready metav1.Condition, pod, scheduler string) metav1.Condition {
	why := strings.TrimSpace(scheduler)
	if len(why) > maxFailureText {
		why = strings.ToValidUTF8(why[:maxFailureText], "") + "..."
	}
	if why == "" {
		why = "kubectl describe pod " + pod + " says why"
	}
	ready.Reason = treebindv1alpha1.ReasonSyncPodUnschedulable
	ready.Message = fmt.Sprintf("%s: the sync pod %s is Pending, because the scheduler cannot place it: %s",
		ready.Message, pod, why)
	return ready
}

// reasonReadinessGatesNotReady is the reason that the kubelet gives the Ready condition of a pod whose containers are
// ready and one of whose readiness gates is not.
const reasonReadinessGatesNotReady = "ReadinessGatesNotReady"

// onNodeThatIsNotReady says whether the pod runs on a node that is not ready, as the pod shows it: its Ready condition
// is False while its ContainersReady condition is True. The kubelet writes both from the same container statuses, and
// writes Ready False with ContainersReady True only for a readiness gate, with a reason of its own. A pod with a
// readiness gate, which a service mesh or a webhook can add, is left out, because whoever owns the gate can hold Ready
// False for a cause of its own. The node lifecycle controller sets Ready to False on every pod of a node whose Ready
// condition is not True, and changes nothing else of the pod, so the last word of the kubelet, that the containers are
// ready, stays (#267). A pod that was not ready already when its node was lost does not show it.
func onNodeThatIsNotReady(pod *corev1.Pod) bool {
	ready, containersReady := podCondition(pod, corev1.PodReady), podCondition(pod, corev1.ContainersReady)
	return pod.Spec.NodeName != "" && len(pod.Spec.ReadinessGates) == 0 && pod.Status.Phase == corev1.PodRunning &&
		ready != nil && ready.Status == corev1.ConditionFalse && ready.Reason != reasonReadinessGatesNotReady &&
		containersReady != nil && containersReady.Status == corev1.ConditionTrue
}

// nodeNotReadyCondition returns the Ready condition of a sync pod whose node is not ready, with the reason
// SyncPodNodeNotReady, the names of the node and the pod, and what to do. A pod on a node that has stopped cannot end,
// so the StatefulSet does not replace it, and the operator's calls to it fail: AgentUnreachable says ConnectionFailed
// after a while, and the cause is no network policy. The taint node.kubernetes.io/out-of-service on a node that is shut
// down lets Kubernetes delete the pod, so that a new one is made.
func nodeNotReadyCondition(ready metav1.Condition, pod, node string) metav1.Condition {
	ready.Reason = treebindv1alpha1.ReasonSyncPodNodeNotReady
	ready.Message = fmt.Sprintf("%s: the node %s of the sync pod %s is not ready, and the pod is stuck there. "+
		"If the node is shut down, give it the taint %s with the effect NoExecute, so that Kubernetes deletes the pod "+
		"and makes a new one", ready.Message, node, pod, corev1.TaintNodeOutOfService)
	return ready
}

// reasonOOMKilled is the reason that the kubelet gives a container that the kernel killed for using more memory than
// its limit.
const reasonOOMKilled = "OOMKilled"

// recentKill is how long after it ended a kill for memory still names the cause for a container that runs again and is
// not ready. The kubelet keeps the last end of a container for the life of the pod, so a kill of hours ago would
// otherwise name the cause of every later wait. It is longer than the longest wait of the kubelet before a restart, 5
// minutes, and a run, so that a container that is killed again on every try is named also while it is up.
const recentKill = 10 * time.Minute

// oomKilledCondition returns the Ready condition of a sync pod that is not ready with the reason SyncPodOOMKilled and the
// memory limit of the sync container, when the container is not ready and was killed for memory (#256): it is ended by
// the kill now, it waits to start again after one, or it runs again after one that ended at most recentKill before now.
// Git needs the memory, on a large file, and every restart tries the same update and is killed again, which
// SyncPodNotReady alone does not say. Any other pod leaves the condition as it is.
func oomKilledCondition(ready metav1.Condition, pod *corev1.Pod, now time.Time) metav1.Condition {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != agentContainerName || status.Ready {
			continue
		}
		if !killedForMemoryLately(status, now) {
			return ready
		}
		cause := "the sync container was killed for memory"
		for _, container := range pod.Spec.Containers {
			if limit, ok := container.Resources.Limits[corev1.ResourceMemory]; ok && container.Name == agentContainerName {
				cause = "the sync container was killed for using more than its memory limit of " + limit.String()
			}
		}
		ready.Reason = treebindv1alpha1.ReasonSyncPodOOMKilled
		ready.Message = fmt.Sprintf("%s: %s (%s)", ready.Message, cause, reasonOOMKilled)
		return ready
	}
	return ready
}

// killedForMemoryLately says whether a container is ended by a kill for memory, waits to start again after one, or runs
// again after one that ended at most recentKill before now.
func killedForMemoryLately(status corev1.ContainerStatus, now time.Time) bool {
	switch {
	case killedForMemory(status.State):
		return true
	case !killedForMemory(status.LastTerminationState):
		return false
	case status.State.Waiting != nil:
		return true
	default:
		return now.Sub(status.LastTerminationState.Terminated.FinishedAt.Time) <= recentKill
	}
}

// killedForMemory says whether a state of a container is its end by the kernel for memory.
func killedForMemory(state corev1.ContainerState) bool {
	return state.Terminated != nil && state.Terminated.Reason == reasonOOMKilled
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
	if r.Metrics == nil {
		return errors.New("the metrics of the Treebinds are required")
	}
	// Without it Ready would not say why the sync pod does not run.
	if r.APIReader == nil {
		return errors.New("a reader of the sync pods is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&treebindv1alpha1.Treebind{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("treebind").
		Complete(r)
}
