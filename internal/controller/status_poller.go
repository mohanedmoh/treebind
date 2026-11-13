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
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/statusclient"
)

const (
	// statusPollInterval is how often the agent of a Treebind is polled while the
	// polls work, and statusPollRetryInterval while they fail. Both are changed by up
	// to 20 % so that Treebinds created together do not come together.
	statusPollInterval      = 30 * time.Second
	statusPollRetryInterval = time.Minute

	// statusUnreachableAfter is how long the polls have to fail in a row before the
	// condition says so (docs/adr/0015).
	statusUnreachableAfter = 3 * time.Minute

	// statusPollWorkers is how many agents are polled at once. A poll that gets no
	// answer takes Timeout, and these bound what a cluster of black-holed agents costs.
	statusPollWorkers = 8

	// statusPollNotReconciledWait is how long to wait for the reconcile of the spec to
	// create the sync objects of a Treebind that it has not been through yet, and
	// statusPollConflictWait how long to wait after the status was changed by someone
	// else while the poller held it.
	statusPollNotReconciledWait = 5 * time.Second
	statusPollConflictWait      = time.Second
)

// AgentReader reads the report of the sync agent of a Treebind.
type AgentReader interface {
	Fetch(ctx context.Context, target statusclient.Target) (*status.Report, error)
}

// StatusPollerReconciler polls the sync agent of every Treebind and says in the
// AgentUnreachable condition whether it can read its report (docs/adr/0015). It is a
// controller of its own, apart from the reconcile of the spec, so that an agent that
// does not answer cannot hold up the creation of anything, and it runs only on the
// leader like the other. It changes nothing but that condition. It writes the status
// with the resourceVersion it read, so a copy that the reconcile of the spec has
// outdated since is a conflict, tried again a second later, and never an overwrite.
//
// It also puts the cluster's public keys where the agent reads them as part of each
// poll, which is cheap when nothing changed, and asks for them to be read again when
// an agent refuses its token, because that is what a rotation of the cluster's keys
// looks like before the new keys have reached the pod.
type StatusPollerReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Agents reads the reports.
	Agents AgentReader
	// StatusKeys puts the keys where the agents read them.
	StatusKeys *StatusKeyPublisher

	// now is time.Now unless a test replaces it.
	now func() time.Time

	// failingSince is when the polls of each Treebind that is failing began to fail.
	// It is in memory: a restart of the operator or a new leader starts the count
	// again, which only delays True by a few minutes.
	mu           sync.Mutex
	failingSince map[types.NamespacedName]time.Time
}

func (r *StatusPollerReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// failingFor records a failed poll of the Treebind and returns how long the polls have
// failed in a row, which is 0 for the first.
func (r *StatusPollerReconciler) failingFor(key types.NamespacedName) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	since, failing := r.failingSince[key]
	if !failing {
		if r.failingSince == nil {
			r.failingSince = make(map[types.NamespacedName]time.Time)
		}
		r.failingSince[key] = now
		return 0
	}
	return now.Sub(since)
}

// recovered forgets that the polls of the Treebind were failing.
func (r *StatusPollerReconciler) recovered(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failingSince, key)
}

// Reconcile polls the agent of one Treebind once, writes AgentUnreachable if it
// changed, and asks to be called again after the time of the next poll.
func (r *StatusPollerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	treebind := &treebindv1alpha1.Treebind{}
	if err := r.Get(ctx, req.NamespacedName, treebind); err != nil {
		if apierrors.IsNotFound(err) {
			r.recovered(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Treebind %s: %w", req.NamespacedName, err)
	}
	if !treebind.DeletionTimestamp.IsZero() {
		r.recovered(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	// The reconcile of the spec creates the Service and the StatefulSet. Until it has
	// been through, the name of the sync pod resolves to nothing, and saying so would
	// be saying nothing true.
	if treebind.Status.ObservedGeneration == 0 {
		return ctrl.Result{RequeueAfter: statusPollNotReconciledWait}, nil
	}

	// The reconcile of the spec logs a failure to put the keys in place. Here it only
	// matters when the agent refuses the token, and is looked at again then.
	if keysErr := r.StatusKeys.Ensure(ctx, r.Client, r.Scheme, treebind); keysErr != nil {
		log.V(1).Info("Could not put the keys where the sync agent reads them", "error", keysErr.Error())
	}
	report, err := r.Agents.Fetch(ctx, statusclient.Target{
		Namespace: treebind.Namespace, Name: treebind.Name, Audience: statusAudience(treebind),
	})
	var outcome pollOutcome
	if err != nil {
		var failure *statusclient.Error
		switch {
		case ctx.Err() != nil:
			// The manager is stopping.
			return ctrl.Result{}, nil
		case !errors.As(err, &failure):
			// The name of the Treebind cannot name a sync pod, which the spec forbids, so
			// this does not happen. It is not a fact about the agent, so it is no condition.
			log.Error(err, "Cannot poll the sync agent")
			return ctrl.Result{RequeueAfter: jittered(statusPollRetryInterval)}, nil
		}
		outcome.failure = failure
		if failure.Kind == statusclient.KindTokenRefused {
			// The keys are what the agent checks the token against. Read them again now,
			// and put them in the ConfigMap if they changed.
			r.StatusKeys.Refresh()
			outcome.keysFailed = r.StatusKeys.Ensure(ctx, r.Client, r.Scheme, treebind) != nil
		}
	} else {
		log.V(1).Info("Read the sync agent's report", "checks", len(report.Checks))
	}

	key := client.ObjectKeyFromObject(treebind)
	var failingFor time.Duration
	if outcome.failure != nil && countsAsFailing(outcome) {
		failingFor = r.failingFor(key)
	} else {
		r.recovered(key)
	}

	previous := meta.FindStatusCondition(treebind.Status.Conditions, treebindv1alpha1.ConditionAgentUnreachable)
	condition := agentUnreachableCondition(treebind, outcome, previous, failingFor)
	if meta.SetStatusCondition(&treebind.Status.Conditions, condition) {
		if err := r.Status().Update(ctx, treebind); err != nil {
			if apierrors.IsConflict(err) {
				// The reconcile of the spec wrote the status meanwhile. The next poll starts
				// from what it wrote.
				return ctrl.Result{RequeueAfter: statusPollConflictWait}, nil
			}
			return ctrl.Result{}, fmt.Errorf("update status of Treebind %s: %w", req.NamespacedName, err)
		}
		log.Info("Reported whether the sync agent can be read",
			"unreachable", condition.Status, "reason", condition.Reason)
	}

	if outcome.failure != nil {
		return ctrl.Result{RequeueAfter: jittered(statusPollRetryInterval)}, nil
	}
	return ctrl.Result{RequeueAfter: jittered(statusPollInterval)}, nil
}

// countsAsFailing says whether a failed poll is the agent failing to answer, which
// is what is counted. A name that does not resolve, the lack of a token, and a token
// that was refused because the keys did not get to the pod are not.
func countsAsFailing(outcome pollOutcome) bool {
	switch outcome.failure.Kind {
	case statusclient.KindNotRunning, statusclient.KindTokenUnavailable:
		return false
	case statusclient.KindTokenRefused:
		return !outcome.keysFailed
	default:
		return true
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *StatusPollerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Agents == nil {
		return errors.New("a reader of the sync agents' reports is required")
	}
	if r.StatusKeys == nil || r.StatusKeys.Source == nil {
		return errors.New("a source of the cluster's public signing keys is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		// The polls schedule themselves. A change of the status, which this controller
		// and the reconcile of the spec both make, must not start one.
		For(&treebindv1alpha1.Treebind{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("treebind-status").
		WithOptions(controller.Options{MaxConcurrentReconciles: statusPollWorkers}).
		Complete(r)
}
