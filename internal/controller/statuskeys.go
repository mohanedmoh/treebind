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
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

const (
	// statusKeysRepublish is how long the ConfigMap of keys is believed to hold what
	// was last written to it. Then it is written again, which puts back a ConfigMap
	// that someone edited or deleted. Nothing watches the ConfigMaps, because the
	// operator may not read them.
	statusKeysRepublish = 10 * time.Minute

	// statusKeysRefresh is how often a Treebind is looked at again for the keys: a
	// rotation of the cluster's keys reaches the agents within about this long, plus
	// the time the kubelet takes to show the new file in the pod.
	statusKeysRefresh = 5 * time.Minute

	// statusKeysRetry is how soon a Treebind is looked at again when the keys could not
	// be put where the agent reads them. It does not change Ready, and nothing else
	// says that the cause has gone, so it is tried again on a short wait.
	statusKeysRetry = time.Minute
)

// The permission the publisher needs is create and patch on ConfigMaps in every
// namespace, and nothing else: no get, list, watch, update or delete, so the
// operator reads no ConfigMap and the garbage collector removes the ones it made.
// It cannot be narrower by name, because create cannot be limited by resourceNames
// and the names follow the Treebinds. A test in rbac_test.go keeps it that way.

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;patch

// errReadKeys is the start of the error of a read of the cluster's keys that failed.
// The reading is shared by every Treebind and the key source logs its own failures,
// so a pass that fails this way does not log it again for each Treebind.
var errReadKeys = errors.New("read the cluster's public signing keys")

// StatusKeyPublisher puts the cluster's public signing keys in the ConfigMap that
// the sync pod of a Treebind mounts, so that the agent can check the operator's
// token (docs/adr/0011, docs/adr/0015).
//
// The operator has no permission to read a ConfigMap, so it cannot look at what is
// there. It remembers what it wrote and when, and writes again when the keys or the
// Treebind changed, or when the copy is old. A ConfigMap that the operator did not
// create is never changed: the patch starts with a test of the owner. It is safe
// for concurrent use.
type StatusKeyPublisher struct {
	// Source is where the keys come from.
	Source KeySource

	// now is time.Now unless a test replaces it.
	now func() time.Time

	mu       sync.Mutex
	written  map[types.NamespacedName]writtenKeys
	prunedAt time.Time
}

// writtenKeys is what the operator last wrote to the ConfigMap of one Treebind.
type writtenKeys struct {
	owner types.UID
	hash  [sha256.Size]byte
	at    time.Time
}

// Ensure makes the ConfigMap of the Treebind hold the cluster's keys. It creates
// it with the Treebind as its controller, so that deleting the Treebind deletes it.
// It does not touch a ConfigMap of that name unless this Treebind is its first
// owner.
func (p *StatusKeyPublisher) Ensure(
	ctx context.Context, c client.Client, scheme *runtime.Scheme, treebind *treebindv1alpha1.Treebind,
) error {
	if p == nil || p.Source == nil {
		return errors.New("no source of the cluster's public signing keys")
	}
	keys, err := p.Source.Keys(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errReadKeys, err)
	}

	name := types.NamespacedName{Namespace: treebind.Namespace, Name: statusKeysConfigMapName(treebind)}
	hash := sha256.Sum256(keys)
	if p.upToDate(name, treebind.UID, hash) {
		return nil
	}

	configMap := desiredStatusKeys(treebind, keys)
	if err := controllerutil.SetControllerReference(treebind, configMap, scheme); err != nil {
		return fmt.Errorf("ensure ConfigMap %s: %w", name, err)
	}
	// The patch comes first. The ConfigMap is made once, and written again every
	// time the keys change or the copy is old, and after a restart or a change of
	// leader for every Treebind at once, so the common call is the patch.
	err = patchOwnedStatusKeys(ctx, c, name, treebind.UID, keys)
	if apierrors.IsNotFound(err) {
		err = c.Create(ctx, configMap)
	}
	if err != nil {
		return fmt.Errorf("ensure ConfigMap %s: %w", name, err)
	}

	p.remember(name, treebind.UID, hash)
	logf.FromContext(ctx).V(1).Info("Wrote the cluster's public signing keys", "configMap", name.String())
	return nil
}

// desiredStatusKeys returns the ConfigMap of keys of a Treebind. The owner
// reference is added by the caller.
func desiredStatusKeys(treebind *treebindv1alpha1.Treebind, keys []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statusKeysConfigMapName(treebind),
			Namespace: treebind.Namespace,
			Labels:    objectLabels(treebind),
		},
		Data: map[string]string{statusKeysFile: string(keys)},
	}
}

// patchOwnedStatusKeys replaces the data of a ConfigMap, if the Treebind with the
// given UID is its first owner, and returns a NotFound error when there is none.
// The test at the start of a JSON patch is checked by the API server in the same
// step as the change, so there is no read to race with, and the operator needs no
// permission to read. A ConfigMap that lists another owner before the Treebind is
// refused as well, which is why the operator makes it with the Treebind as its
// only owner.
func patchOwnedStatusKeys(
	ctx context.Context, c client.Client, name types.NamespacedName, owner types.UID, keys []byte,
) error {
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/ownerReferences/0/uid", "value": owner},
		{"op": "add", "path": "/data", "value": map[string]string{statusKeysFile: string(keys)}},
	})
	if err != nil {
		return fmt.Errorf("encode the patch: %w", err)
	}
	target := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}}
	err = c.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch))
	if apierrors.IsInvalid(err) {
		// What the patch changes is valid, so a refusal is almost always the test, but
		// an admission webhook can refuse too, and its reason is in err.
		return fmt.Errorf("a ConfigMap of that name exists, and the operator changes only one whose first owner "+
			"is this Treebind; delete it to let the operator create its own: %w", err)
	}
	return err
}

func (p *StatusKeyPublisher) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// upToDate reports whether the ConfigMap was written with these keys for this
// Treebind, and not so long ago that it should be written again.
func (p *StatusKeyPublisher) upToDate(name types.NamespacedName, owner types.UID, hash [sha256.Size]byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	last, ok := p.written[name]
	return ok && last.owner == owner && last.hash == hash && p.clock().Sub(last.at) < statusKeysRepublish
}

// remember records a write, and forgets the Treebinds that have not been written
// for long, so that deleted ones do not stay for ever.
func (p *StatusKeyPublisher) remember(name types.NamespacedName, owner types.UID, hash [sha256.Size]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.written == nil {
		p.written = make(map[types.NamespacedName]writtenKeys)
	}
	p.written[name] = writtenKeys{owner: owner, hash: hash, at: now}

	if now.Sub(p.prunedAt) >= statusKeysRepublish {
		p.prunedAt = now
		for key, last := range p.written {
			if now.Sub(last.at) >= 2*statusKeysRepublish {
				delete(p.written, key)
			}
		}
	}
}

// jittered returns d changed by up to 20 % either way, so that the Treebinds that
// were created together do not all come back at the same time.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) / 5
	return d - time.Duration(spread) + time.Duration(rand.Int64N(2*spread+1))
}
