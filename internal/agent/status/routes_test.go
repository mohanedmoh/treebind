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

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// routes is the handler of an agent that is alive, with the readiness that the test gives.
func routes(ready bool) http.Handler { return routesFor(ready, true) }

func routesFor(ready, alive bool) http.Handler {
	return Routes(newHandler(onlyGoodToken), func() bool { return ready }, func() bool { return alive })
}

func get(handler http.Handler, method, path string, authorization ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for _, value := range authorization {
		r.Header.Add("Authorization", value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestRoutesServeTheReportOnlyToAnAcceptedCaller(t *testing.T) {
	if w := get(routes(false), http.MethodGet, Path, "Bearer "+goodToken); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"name":"dashboards"`) {
		t.Errorf("an accepted caller got %d: %s", w.Code, w.Body.String())
	}
	if w := get(routes(true), http.MethodGet, Path); w.Code != http.StatusUnauthorized {
		t.Errorf("a stranger got %d, want 401", w.Code)
	}
}

func TestProbes(t *testing.T) {
	t.Run("healthz is ok for a loop that is alive, whatever the readiness", func(t *testing.T) {
		for _, ready := range []bool{false, true} {
			w := get(routes(ready), http.MethodGet, HealthPath)

			if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
				t.Errorf("ready %v: %d %q", ready, w.Code, w.Body.String())
			}
		}
	})

	t.Run("healthz fails for a loop that is stuck, whatever the readiness", func(t *testing.T) {
		for _, ready := range []bool{false, true} {
			w := get(routesFor(ready, false), http.MethodGet, HealthPath)

			// Not "not ready": a stuck loop is not about readiness, and the word is what a person finds in the
			// events of the pod.
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != "not alive\n" {
				t.Errorf("ready %v: %d %q, want 503 and not alive", ready, w.Code, w.Body.String())
			}
		}
		if w := get(routesFor(true, false), http.MethodHead, HealthPath); w.Code != http.StatusServiceUnavailable {
			t.Errorf("HEAD: %d, want 503", w.Code)
		}
	})

	t.Run("readyz does not follow the liveness, and healthz does not follow the readiness", func(t *testing.T) {
		if w := get(routesFor(true, false), http.MethodGet, ReadyPath); w.Code != http.StatusOK {
			t.Errorf("ready and stuck: readyz is %d, want 200: the loop being late is not what readiness is about", w.Code)
		}
		if w := get(routesFor(false, true), http.MethodGet, HealthPath); w.Code != http.StatusOK {
			t.Errorf("not ready and alive: healthz is %d, want 200", w.Code)
		}
	})

	t.Run("readyz follows the readiness", func(t *testing.T) {
		if w := get(routes(true), http.MethodGet, ReadyPath); w.Code != http.StatusOK || w.Body.String() != "ready\n" {
			t.Errorf("ready: %d %q", w.Code, w.Body.String())
		}
		w := get(routes(false), http.MethodGet, ReadyPath)
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != "not ready\n" {
			t.Errorf("not ready: %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("a probe needs no token and says nothing but its word", func(t *testing.T) {
		for _, path := range []string{HealthPath, ReadyPath} {
			w := get(routes(true), http.MethodGet, path)

			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "dashboards") {
				t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
			}
		}
	})

	t.Run("a HEAD gets the status and no body", func(t *testing.T) {
		if w := get(routes(true), http.MethodHead, ReadyPath); w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Errorf("%d with %d bytes", w.Code, w.Body.Len())
		}
		if w := get(routes(false), http.MethodHead, ReadyPath); w.Code != http.StatusServiceUnavailable {
			t.Errorf("not ready: %d", w.Code)
		}
	})

	t.Run("nothing but GET and HEAD", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			w := get(routes(true), method, ReadyPath)

			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s: %d, Allow %q", method, w.Code, w.Header().Get("Allow"))
			}
		}
	})

	t.Run("no answer may be cached", func(t *testing.T) {
		for _, path := range []string{HealthPath, ReadyPath, "/nothing"} {
			for _, ready := range []bool{false, true} {
				for _, alive := range []bool{false, true} {
					if got := get(routesFor(ready, alive), http.MethodGet, path).Header().Get("Cache-Control"); got != noStore {
						t.Errorf("%s, ready %v, alive %v: Cache-Control is %q", path, ready, alive, got)
					}
				}
			}
		}
	})
}

func TestRoutesServeNothingElse(t *testing.T) {
	for _, path := range []string{
		"/", "/healthz/", "/readyz/x", "/HEALTHZ", "//healthz", "/%68ealthz", "/status/", "/metrics", "/debug/pprof/",
	} {
		w := get(routes(true), http.MethodGet, path, "Bearer "+goodToken)

		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}

func TestRoutesNeedAHandlerAReadinessFunctionAndALivenessFunction(t *testing.T) {
	yes := func() bool { return true }
	for name, build := range map[string]func(){
		"no report handler": func() { Routes(nil, yes, yes) },
		"no readiness":      func() { Routes(newHandler(onlyGoodToken), nil, yes) },
		"no liveness":       func() { Routes(newHandler(onlyGoodToken), yes, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Routes did not panic")
				}
			}()
			build()
		})
	}
}
