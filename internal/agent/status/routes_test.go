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

func routes(ready bool) http.Handler {
	return Routes(newHandler(onlyGoodToken), func() bool { return ready })
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
	t.Run("healthz is ok whatever the readiness", func(t *testing.T) {
		for _, ready := range []bool{false, true} {
			w := get(routes(ready), http.MethodGet, HealthPath)

			if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
				t.Errorf("ready %v: %d %q", ready, w.Code, w.Body.String())
			}
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
				if got := get(routes(ready), http.MethodGet, path).Header().Get("Cache-Control"); got != noStore {
					t.Errorf("%s, ready %v: Cache-Control is %q", path, ready, got)
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

func TestRoutesNeedAHandlerAndAReadinessFunction(t *testing.T) {
	for name, build := range map[string]func(){
		"no report handler": func() { Routes(nil, func() bool { return true }) },
		"no readiness":      func() { Routes(newHandler(onlyGoodToken), nil) },
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
