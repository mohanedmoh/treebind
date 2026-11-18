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
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// ParseWatchNamespaces reads the value of the operator's --watch-namespaces: the names of the namespaces
// whose Treebinds it serves, separated by commas, with blanks around a name left out and a name that comes
// twice kept once. Nothing, or only blanks, means every namespace, and is a nil list. A name that is not the
// name of a namespace, or an empty one between two commas, is an error: a list that is read leniently would
// be a Role that does not cover what the operator watches, which shows only as errors of the cache.
func ParseWatchNamespaces(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var names []string
	for item := range strings.SplitSeq(value, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			return nil, fmt.Errorf("the list of namespaces %q has an empty name in it", value)
		}
		if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
			return nil, fmt.Errorf("%q is not the name of a namespace: %s", name, strings.Join(problems, "; "))
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// CacheOptions limits the cache of the manager to the namespaces, so that the operator never asks the API
// server for a Treebind, a StatefulSet or a Service outside them, which the Roles of a namespaced install do
// not allow (docs/rbac.md). With none it leaves the cache open to every namespace, as the ClusterRole of a
// cluster-wide install allows.
func CacheOptions(namespaces []string) cache.Options {
	if len(namespaces) == 0 {
		return cache.Options{}
	}
	limited := make(map[string]cache.Config, len(namespaces))
	for _, name := range namespaces {
		limited[name] = cache.Config{}
	}
	return cache.Options{DefaultNamespaces: limited}
}
