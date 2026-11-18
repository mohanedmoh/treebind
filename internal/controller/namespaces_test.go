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
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The names of namespaces that the tests of the list use.
const (
	nsApps   = "apps"
	nsTools  = "tools"
	wordNone = "empty"
)

func TestParseWatchNamespaces(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
		fails string
	}{
		{name: "nothing means every namespace", value: ""},
		{name: "only blanks mean every namespace", value: "  "},
		{name: "one", value: nsApps, want: []string{nsApps}},
		{name: "two", value: nsApps + "," + nsTools, want: []string{nsApps, nsTools}},
		{name: "blanks around a name are dropped", value: " " + nsApps + " , " + nsTools + " ", want: []string{nsApps, nsTools}},
		{name: "a name that comes twice is kept once, where it first came", value: nsApps + "," + nsTools + "," + nsApps, want: []string{nsApps, nsTools}},
		{name: "a name of 63 characters", value: strings.Repeat("a", 63), want: []string{strings.Repeat("a", 63)}},
		{name: "digits and dashes inside", value: "team-1,2b", want: []string{"team-1", "2b"}},
		{name: "an empty name in the middle", value: nsApps + ",," + nsTools, fails: wordNone},
		{name: "a comma at the end", value: nsApps + ",", fails: wordNone},
		{name: "a comma at the start", value: "," + nsApps, fails: wordNone},
		{name: "a name with a capital", value: "Apps", fails: `"Apps"`},
		{name: "a name with a dot", value: nsApps + "." + nsTools, fails: `"apps.tools"`},
		{name: "a name that starts with a dash", value: "-" + nsApps, fails: `"-apps"`},
		{name: "a name that ends with a dash", value: nsApps + "-", fails: `"apps-"`},
		{name: "a name of 64 characters", value: strings.Repeat("a", 64), fails: "63"},
		{name: caseWildcard, value: "*", fails: `"*"`},
		{name: "a slash", value: nsApps + "/" + nsTools, fails: `"apps/tools"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWatchNamespaces(tt.value)
			if tt.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tt.fails) {
					t.Fatalf("ParseWatchNamespaces(%q) = %v, %v, want an error that holds %q", tt.value, got, err, tt.fails)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWatchNamespaces(%q) failed: %v", tt.value, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseWatchNamespaces(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestCacheOptionsLimitTheCacheToTheNamespacesAndLeaveItOpenWithNone(t *testing.T) {
	if got := CacheOptions(nil); len(got.DefaultNamespaces) != 0 {
		t.Errorf("CacheOptions(nil) limits the cache to %v, want it open to every namespace", got.DefaultNamespaces)
	}
	if got := CacheOptions([]string{}); len(got.DefaultNamespaces) != 0 {
		t.Errorf("CacheOptions of an empty list limits the cache to %v, want it open to every namespace", got.DefaultNamespaces)
	}
	got := CacheOptions([]string{nsApps, nsTools})
	if len(got.DefaultNamespaces) != 2 {
		t.Fatalf("CacheOptions limits the cache to %v, want apps and tools", got.DefaultNamespaces)
	}
	for _, name := range []string{nsApps, nsTools} {
		if _, ok := got.DefaultNamespaces[name]; !ok {
			t.Errorf("CacheOptions does not hold %q: %v", name, got.DefaultNamespaces)
		}
	}
}

// A cache that is limited to namespaces is what keeps the operator within the Roles that it has: it never asks
// the API server for a Treebind outside them, which a Role in one namespace would refuse. This runs the real
// cache against the API server of envtest. That API server lets everything through, so what is held here is
// that the cache does not ask, and the cluster check of #30 holds that the Roles are enough.
var _ = Describe("The cache of the operator when it watches namespaces", func() {
	const (
		watched = "watch-ns-a"
		other   = "watch-ns-b"
	)
	ctx := context.Background()

	treebindIn := func(namespace string) *treebindv1alpha1.Treebind {
		treebind := newTreebind("in-" + namespace)
		treebind.Namespace = namespace
		return treebind
	}

	BeforeEach(func() {
		for _, name := range []string{watched, other} {
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("already exists"))
			}
			err = k8sClient.Create(ctx, treebindIn(name))
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("already exists"))
			}
		}
	})

	// startCache starts a cache that is limited to the namespaces, as the operator's is, and stops it at the
	// end of the spec.
	startCache := func(namespaces []string) cache.Cache {
		GinkgoHelper()
		cacheCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		options := CacheOptions(namespaces)
		options.Scheme = scheme.Scheme
		c, err := cache.New(cfg, options)
		Expect(err).NotTo(HaveOccurred())
		go func() {
			defer GinkgoRecover()
			Expect(c.Start(cacheCtx)).To(Succeed())
		}()
		Expect(c.WaitForCacheSync(cacheCtx)).To(BeTrue(), "the cache did not start")
		return c
	}

	// namesIn lists, through a cache limited to the namespaces, the Treebinds that the test made, as
	// namespace/name.
	namesIn := func(namespaces []string) func() ([]string, error) {
		c := startCache(namespaces)
		return func() ([]string, error) {
			var list treebindv1alpha1.TreebindList
			if err := c.List(ctx, &list); err != nil {
				return nil, err
			}
			var names []string
			for _, item := range list.Items {
				if item.Namespace == watched || item.Namespace == other {
					names = append(names, item.Namespace+"/"+item.Name)
				}
			}
			slices.Sort(names)
			return names, nil
		}
	}

	It("reads the Treebinds of the namespaces it is limited to and no others", func() {
		list := namesIn([]string{watched})

		Eventually(list, "10s").Should(Equal([]string{watched + "/in-" + watched}))
		Consistently(list, "1s").Should(Equal([]string{watched + "/in-" + watched}),
			"the cache of the operator read a Treebind of a namespace that it is not limited to")
	})

	It("reads the Treebinds of every namespace when it is not limited", func() {
		list := namesIn(nil)

		Eventually(list, "10s").Should(Equal([]string{watched + "/in-" + watched, other + "/in-" + other}))
	})

	It("refuses a read of a Treebind outside the namespaces, and does not take one for missing", func() {
		c := startCache([]string{watched})

		var inside, outside treebindv1alpha1.Treebind
		Eventually(func() error {
			return c.Get(ctx, types.NamespacedName{Namespace: watched, Name: "in-" + watched}, &inside)
		}, "10s").Should(Succeed())
		Expect(c.Get(ctx, types.NamespacedName{Namespace: other, Name: "in-" + other}, &outside)).NotTo(Succeed())
	})
})
