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

package status

import "net/http"

const (
	// HealthPath answers whether the agent is running.
	HealthPath = "/healthz"
	// ReadyPath answers whether the agent is ready: it has synced.
	ReadyPath = "/readyz"
)

// Routes returns the handler of everything the agent serves: the report at Path
// for a caller with a token, and the two probe paths for the kubelet, which cannot
// present one. A probe says "ok" or "not ready" and nothing else, so it tells a
// stranger nothing the kubelet's own view of the pod does not. Any other path is
// not found, and the path is compared as it was sent, with nothing cleaned,
// decoded or redirected.
func Routes(report http.Handler, ready func() bool) http.Handler {
	if report == nil || ready == nil {
		panic("status: Routes needs a report handler and a readiness function")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case Path:
			report.ServeHTTP(w, r)
		case HealthPath:
			probe(w, r, true, "ok")
		case ReadyPath:
			probe(w, r, ready(), "ready")
		default:
			w.Header().Set("Cache-Control", noStore)
			http.NotFound(w, r)
		}
	})
}

// noStore keeps every answer of the endpoint out of caches.
const noStore = "no-store"

// probe answers a probe request with the word when the probe passes and with 503
// when it does not.
func probe(w http.ResponseWriter, r *http.Request, passes bool, word string) {
	w.Header().Set("Cache-Control", noStore)
	if !readOnlyMethod(w, r) {
		return
	}
	if !passes {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(word + "\n"))
	}
}
