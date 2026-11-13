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
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/auth"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// foreignData is the content of a ConfigMap that the operator did not make.
const foreignData = "somebody else's"

// staticKeys is a KeySource that returns what a test says.
type staticKeys struct {
	mu   sync.Mutex
	keys []byte
	err  error
}

func (s *staticKeys) set(keys []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys, s.err = keys, err
}

func (s *staticKeys) Keys(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys, s.err
}

// newTestPublisher returns a publisher whose keys are the test key set.
func newTestPublisher() *StatusKeyPublisher {
	source := &staticKeys{}
	source.set(testKeySet(), nil)
	return &StatusKeyPublisher{Source: source}
}

// signToken signs a token the way the API server does for a service account: RS256
// with the key ID in the header.
func signToken(t testing.TB, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	segment := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	signed := segment(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"}) + "." + segment(claims)
	sum := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// The agent reads the keys from a file in its own pod, and the operator writes
// them to a ConfigMap. Nothing but this test says that the two agree: that the
// pod mounts the ConfigMap the operator writes, that the file the agent is told to
// read is the key that the ConfigMap holds, and that the agent's own verifier
// accepts what the operator publishes.
func TestTheAgentAcceptsTheKeysTheOperatorPublishes(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Namespace = testNamespace
	configMap := desiredStatusKeys(treebind, testKeySet())
	container, pod := agentContainer(t, treebind)

	env := map[string]string{}
	for _, variable := range container.Env {
		env[variable.Name] = variable.Value
		if variable.ValueFrom != nil {
			env[variable.Name] = testNamespace // metadata.namespace, the only field the pod reads
		}
	}
	config, err := agentconfig.FromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}

	// The mount that holds the file the agent reads, and the volume behind it.
	var mount *corev1.VolumeMount
	for i, candidate := range container.VolumeMounts {
		if strings.HasPrefix(config.StatusKeysFile, candidate.MountPath+"/") {
			mount = &container.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("the agent reads %s, which no mount of the pod holds", config.StatusKeysFile)
	}
	var volume *corev1.Volume
	for i, candidate := range pod.Volumes {
		if candidate.Name == mount.Name {
			volume = &pod.Volumes[i]
		}
	}
	if volume == nil || volume.ConfigMap == nil || volume.ConfigMap.Name != configMap.Name {
		t.Fatalf("the pod mounts %+v, want the ConfigMap %s that the operator writes", volume, configMap.Name)
	}

	// The files as the kubelet lays them out: one per item, named by its path.
	dir := t.TempDir()
	for _, item := range volume.ConfigMap.Items {
		content, ok := configMap.Data[item.Key]
		if !ok {
			t.Fatalf("the pod mounts the key %q, which the ConfigMap does not hold", item.Key)
		}
		if err := os.WriteFile(filepath.Join(dir, item.Path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keysFile := filepath.Join(dir, strings.TrimPrefix(config.StatusKeysFile, mount.MountPath+"/"))

	verifier, err := auth.New(auth.Config{
		Issuer: config.StatusIssuer, Audience: config.StatusAudience, Subject: config.StatusSubject, KeysFile: keysFile,
	})
	if err != nil {
		t.Fatal(err)
	}

	claims := func(audience string) map[string]any {
		return map[string]any{
			"iss": testIssuer, "sub": testSubject, "aud": []string{audience},
			"iat": time.Now().Unix(), "nbf": time.Now().Unix(), "exp": time.Now().Add(10 * time.Minute).Unix(),
		}
	}
	good := signToken(t, testSigningKey(), testKeyID, claims(statusAudience(treebind)))
	if err := verifier.Verify(good); err != nil {
		t.Errorf("the agent refuses the operator's token with the keys the operator published: %v", err)
	}

	// The same file refuses what it should, which shows that the check above is the
	// agent's and that the keys are what decided it.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(signToken(t, other, testKeyID, claims(statusAudience(treebind)))); err == nil {
		t.Error("the agent accepts a token signed by a key that is not in the published set")
	}
	if err := verifier.Verify(signToken(t, testSigningKey(), testKeyID, claims("treebind.example.org/apps/other"))); err == nil {
		t.Error("the agent accepts a token for the audience of another Treebind")
	}
}

func TestTheOperatorWillNotStartWithoutAKeySource(t *testing.T) {
	for name, keys := range map[string]*StatusKeyPublisher{
		"no publisher":           nil,
		"a publisher of nothing": {},
	} {
		reconciler := &TreebindReconciler{StatusIssuer: testIssuer, StatusSubject: testSubject, StatusKeys: keys}
		// The manager is nil, so an error of the controller builder would pass for
		// the refusal: the text has to be the operator's own.
		if err := reconciler.SetupWithManager(nil); err == nil || !strings.Contains(err.Error(), "public signing keys") {
			t.Errorf("%s: SetupWithManager returned %v, want the refusal of the operator", name, err)
		}
	}
}

func TestEnsureRefusesWithoutAKeySource(t *testing.T) {
	var none *StatusKeyPublisher
	for name, publisher := range map[string]*StatusKeyPublisher{"no publisher": none, "no source": {}} {
		err := publisher.Ensure(context.Background(), nil, nil, newTreebind(testTreebindName))
		if err == nil || !strings.Contains(err.Error(), "no source of the cluster's public signing keys") {
			t.Errorf("%s: Ensure returned %v", name, err)
		}
	}
}

func TestRefreshOfAPublisherReachesTheSourceThatCanRefresh(t *testing.T) {
	api, clock := &fakeAPI{}, newFakeClock()
	api.set(testKeySet(), nil)
	fetcher := newTestFetcher(api, clock)
	if _, err := fetcher.Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(keysMinRefresh)

	(&StatusKeyPublisher{Source: fetcher}).Refresh()
	if _, err := fetcher.Keys(context.Background()); err != nil || api.calls.Load() != 2 {
		t.Errorf("the source was not asked to read again: %v after %d reads", err, api.calls.Load())
	}

	// A source that cannot refresh, and no publisher at all, are not an error.
	(&StatusKeyPublisher{Source: &staticKeys{}}).Refresh()
	(*StatusKeyPublisher)(nil).Refresh()
	(&StatusKeyPublisher{}).Refresh()
}

func TestJitterStaysWithinAFifthEitherWay(t *testing.T) {
	const base = 5 * time.Minute
	low, high := base, base
	for range 2000 {
		got := jittered(base)
		if got < base*4/5 || got > base*6/5 {
			t.Fatalf("jittered(%v) = %v, want within 20%% either way", base, got)
		}
		low, high = min(low, got), max(high, got)
	}
	if low >= base || high <= base {
		t.Errorf("2000 draws ranged from %v to %v, want some on each side of %v: the Treebinds would still come together",
			low, high, base)
	}
	// A duration too short to have a fifth, or that is not a duration to wait, must
	// not panic.
	for _, d := range []time.Duration{-time.Minute, -1, 0, 1, 4} {
		if got := jittered(d); got != d {
			t.Errorf("jittered(%v) = %v, want it unchanged", d, got)
		}
	}
}

func TestThePublisherForgetsTreebindsThatWereNotWrittenForLong(t *testing.T) {
	clock := newFakeClock()
	publisher := &StatusKeyPublisher{now: clock.Now}
	name := func(n string) types.NamespacedName { return types.NamespacedName{Namespace: testNamespace, Name: n} }
	var hash [sha256.Size]byte
	remembered := func(n string) bool { _, ok := publisher.written[name(n)]; return ok }

	// The forgetting runs at most once per republish time, so the writes are spread
	// to make it run at the second and the fourth of them.
	publisher.remember(name("old"), "uid-1", hash)
	clock.Advance(statusKeysRepublish - time.Second)
	publisher.remember(name("edge"), "uid-2", hash)
	clock.Advance(statusKeysRepublish)
	publisher.remember(name("recent"), "uid-3", hash)
	if !remembered("old") || !remembered("edge") {
		t.Fatal("a Treebind written less than twice the republish time ago was forgotten")
	}

	clock.Advance(statusKeysRepublish)
	publisher.remember(name("new"), "uid-4", hash)
	if remembered("old") {
		t.Error("a Treebind written three times the republish time ago is still remembered, so deleted ones pile up")
	}
	if remembered("edge") {
		t.Error("a Treebind written exactly twice the republish time ago is still remembered")
	}
	if !remembered("recent") || !remembered("new") {
		t.Error("a Treebind written within the republish time was forgotten")
	}
}

func TestThePublisherDoesNotWalkItsMemoryOnEveryWrite(t *testing.T) {
	clock := newFakeClock()
	publisher := &StatusKeyPublisher{now: clock.Now}
	name := func(n string) types.NamespacedName { return types.NamespacedName{Namespace: testNamespace, Name: n} }
	var hash [sha256.Size]byte

	publisher.remember(name("old"), "uid-1", hash) // the first write forgets, and sets the time of the next
	clock.Advance(statusKeysRepublish + time.Second)
	publisher.remember(name("again"), "uid-2", hash) // so does this one, a second over the republish time later
	clock.Advance(statusKeysRepublish - time.Second)
	publisher.remember(name("new"), "uid-3", hash)

	// "old" is exactly twice the republish time old now, which is old enough to be
	// forgotten, but the last forgetting was a second under the republish time ago.
	if _, kept := publisher.written[name("old")]; !kept {
		t.Error("the publisher walked its memory again less than the republish time after it last did")
	}

	// A second later the republish time has passed since it last did.
	clock.Advance(time.Second)
	publisher.remember(name("newer"), "uid-4", hash)
	if _, kept := publisher.written[name("old")]; kept {
		t.Error("the publisher did not walk its memory once the republish time had passed")
	}
}

var _ = Describe("The ConfigMap of keys", func() {
	const namespace = "default"

	var (
		ctx       = context.Background()
		clock     *fakeClock
		source    *staticKeys
		publisher *StatusKeyPublisher
		writes    *writeLog
		cl        client.Client
		treebind  *treebindv1alpha1.Treebind
		name      types.NamespacedName
		counter   int
	)

	BeforeEach(func() {
		counter++
		clock = newFakeClock()
		source = &staticKeys{}
		source.set(testKeySet(), nil)
		publisher = &StatusKeyPublisher{Source: source, now: clock.Now}

		writes = &writeLog{}
		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		cl = interceptor.NewClient(watching, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				writes.add(func(w *writeCounts) { w.creates++ })
				return c.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption,
			) error {
				writes.add(func(w *writeCounts) { w.patches++ })
				return c.Patch(ctx, obj, patch, opts...)
			},
		})

		treebind = newTreebind(fmt.Sprintf("keys-%d", counter))
		Expect(k8sClient.Create(ctx, treebind)).To(Succeed())
		name = types.NamespacedName{Namespace: namespace, Name: treebind.Name + "-sync-keys"}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, treebind))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}}))).To(Succeed())
	})

	ensure := func() error {
		GinkgoHelper()
		return publisher.Ensure(ctx, cl, k8sClient.Scheme(), treebind)
	}
	get := func() *corev1.ConfigMap {
		GinkgoHelper()
		got := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, name, got)).To(Succeed())
		return got
	}

	It("is created with the keys under the name the pod mounts, and the Treebind as its owner", func() {
		Expect(ensure()).To(Succeed())

		got := get()
		Expect(got.Data).To(Equal(map[string]string{statusKeysFile: string(testKeySet())}))
		Expect(got.Labels).To(Equal(objectLabels(treebind)))
		ref := metav1.GetControllerOf(got)
		Expect(ref).NotTo(BeNil())
		Expect(ref.UID).To(Equal(treebind.UID))
		Expect(ref.Kind).To(Equal("Treebind"))
		Expect(ref.Name).To(Equal(treebind.Name))
		Expect(ref.BlockOwnerDeletion).To(HaveValue(BeTrue()))
		Expect(got.OwnerReferences).To(HaveLen(1))
	})

	It("is not written again while the keys are the same and the copy is not old", func() {
		Expect(ensure()).To(Succeed())
		clock.Advance(statusKeysRepublish - time.Second)
		Expect(ensure()).To(Succeed())

		Expect(writes.get()).To(Equal(writeCounts{creates: 1, patches: 1}))
	})

	It("is one patch and no create for a publisher that has no memory of it, as after a restart", func() {
		Expect(ensure()).To(Succeed())
		before := writes.get()

		restarted := &StatusKeyPublisher{Source: source, now: clock.Now}
		Expect(restarted.Ensure(ctx, cl, k8sClient.Scheme(), treebind)).To(Succeed())

		after := writes.get()
		Expect(after.creates).To(Equal(before.creates), "a create that fails with AlreadyExists for every Treebind at once")
		Expect(after.patches).To(Equal(before.patches + 1))
	})

	It("patches after all when the ConfigMap was made between its patch and its create", func() {
		Expect(ensure()).To(Succeed())
		rotated := jwksOf("rotated", &testSigningKey().PublicKey)
		source.set(rotated, nil)

		// The first patch of a publisher that has no memory of the ConfigMap finds none,
		// as it does when the other controller makes it in that moment.
		var patches atomic.Int32
		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		raced := interceptor.NewClient(watching, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if patches.Add(1) == 1 {
					return apierrors.NewNotFound(corev1.Resource("configmaps"), obj.GetName())
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		other := &StatusKeyPublisher{Source: source, now: clock.Now}

		Expect(other.Ensure(ctx, raced, k8sClient.Scheme(), treebind)).To(Succeed())

		Expect(get().Data).To(Equal(map[string]string{statusKeysFile: string(rotated)}))
		Expect(patches.Load()).To(Equal(int32(2)))
	})

	It("is written again when the keys change, without waiting", func() {
		Expect(ensure()).To(Succeed())
		// The first time there is nothing to patch, so the ConfigMap is made.
		Expect(writes.get()).To(Equal(writeCounts{creates: 1, patches: 1}))
		changed := jwksOf("rotated", &testSigningKey().PublicKey)
		source.set(changed, nil)
		Expect(ensure()).To(Succeed())

		// A later write is one patch.
		Expect(writes.get()).To(Equal(writeCounts{creates: 1, patches: 2}))
		Expect(get().Data).To(Equal(map[string]string{statusKeysFile: string(changed)}))
	})

	It("is written again once the copy is old, which puts back what someone edited", func() {
		Expect(ensure()).To(Succeed())
		edited := get()
		edited.Data = map[string]string{statusKeysFile: "edited by hand", "extra": "x"}
		Expect(k8sClient.Update(ctx, edited)).To(Succeed())

		clock.Advance(statusKeysRepublish)
		Expect(ensure()).To(Succeed())

		Expect(get().Data).To(Equal(map[string]string{statusKeysFile: string(testKeySet())}))
	})

	It("is written again once the copy is old, which puts back a ConfigMap that someone emptied", func() {
		Expect(ensure()).To(Succeed())
		emptied := get()
		emptied.Data = nil
		Expect(k8sClient.Update(ctx, emptied)).To(Succeed())
		Expect(get().Data).To(BeEmpty())

		clock.Advance(statusKeysRepublish)
		Expect(ensure()).To(Succeed())

		Expect(get().Data).To(Equal(map[string]string{statusKeysFile: string(testKeySet())}))
	})

	It("is created again once the copy is old, if someone deleted it", func() {
		Expect(ensure()).To(Succeed())
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())

		clock.Advance(statusKeysRepublish - time.Second)
		Expect(ensure()).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, name, &corev1.ConfigMap{}))).To(BeTrue(),
			"it was written before the copy was old")

		clock.Advance(time.Second)
		Expect(ensure()).To(Succeed())
		Expect(get().Data).To(HaveKey(statusKeysFile))
	})

	It("is written for a Treebind that was created again under the same name, whatever was written before", func() {
		Expect(ensure()).To(Succeed())
		Expect(k8sClient.Delete(ctx, get())).To(Succeed()) // the garbage collector does this in a cluster
		Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())

		again := newTreebind(treebind.Name)
		Expect(k8sClient.Create(ctx, again)).To(Succeed())
		Expect(again.UID).NotTo(Equal(treebind.UID))
		treebind = again
		Expect(ensure()).To(Succeed())

		Expect(metav1.GetControllerOf(get()).UID).To(Equal(again.UID))
	})

	It("does not change a ConfigMap of that name that nobody owns", func() {
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
			Data:       map[string]string{statusKeysFile: foreignData},
		})).To(Succeed())

		err := ensure()

		Expect(err).To(MatchError(ContainSubstring("ensure ConfigMap " + name.String())))
		Expect(err).To(MatchError(ContainSubstring("first owner is this Treebind")))
		Expect(get().Data).To(Equal(map[string]string{statusKeysFile: foreignData}))
	})

	It("does not change a ConfigMap of that name that something else owns", func() {
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name.Name, Namespace: name.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "ConfigMap", Name: "someone", UID: "00000000-0000-0000-0000-000000000042",
				}},
			},
			Data: map[string]string{statusKeysFile: foreignData},
		})).To(Succeed())

		Expect(ensure()).To(MatchError(ContainSubstring("first owner is this Treebind")))
		Expect(get().Data).To(Equal(map[string]string{statusKeysFile: foreignData}))
	})

	It("is not written, and says why, when the keys cannot be read", func() {
		source.set(nil, errors.New("the API server said no"))

		err := ensure()

		Expect(err).To(MatchError(ContainSubstring("read the cluster's public signing keys: the API server said no")))
		Expect(writes.get()).To(Equal(writeCounts{}))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, name, &corev1.ConfigMap{}))).To(BeTrue())
	})
})

// writeCounts is how many ConfigMaps the publisher created and patched.
type writeCounts struct{ creates, patches int }

// writeLog counts writes from several goroutines.
type writeLog struct {
	mu sync.Mutex
	c  writeCounts
}

func (w *writeLog) add(change func(*writeCounts)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	change(&w.c)
}

func (w *writeLog) get() writeCounts {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c
}
