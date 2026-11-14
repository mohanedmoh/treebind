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

package main

import (
	"bytes"
	"context"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/config"
)

func environment(extra map[string]string) func(string) string {
	env := map[string]string{
		config.EnvNamespace:      "apps",
		config.EnvName:           "dashboards",
		config.EnvStatusIssuer:   "https://kubernetes.default.svc.cluster.local",
		config.EnvStatusAudience: "treebind.example.org/apps/dashboards",
		config.EnvStatusSubject:  "system:serviceaccount:treebind-system:treebind-controller-manager",
		config.EnvStatusAddr:     "127.0.0.1:0",
		config.EnvMountPath:      "/work",
		config.EnvRepoURL:        "https://example.com/acme/dashboards.git",
	}
	maps.Copy(env, extra)
	return func(name string) string { return env[name] }
}

func TestRunRefusesAWrongConfiguration(t *testing.T) {
	var stderr bytes.Buffer

	code := run(context.Background(), func(string) string { return "" }, &stderr)

	if code != exitConfig {
		t.Errorf("exit code %d, want %d", code, exitConfig)
	}
	for _, want := range []string{
		"configuration is wrong", config.EnvNamespace, config.EnvStatusIssuer, config.EnvStatusSubject, config.EnvRepoURL,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the message does not say %q:\n%s", want, stderr.String())
		}
	}
}

// lockedBuffer is a destination that the agent writes to while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunStopsOnRequestWithCodeZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, environment(map[string]string{config.EnvMountPath: t.TempDir()}), &stderr)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "serving the status endpoint") {
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not start serving:\n%s", stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()

	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("exit code %d, want %d:\n%s", code, exitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
}

func TestRunFailsWhenItCannotServe(t *testing.T) {
	var stderr bytes.Buffer

	code := run(context.Background(), environment(map[string]string{config.EnvStatusAddr: "256.0.0.1:1"}), &stderr)

	if code != exitFailed {
		t.Errorf("exit code %d, want %d:\n%s", code, exitFailed, stderr.String())
	}
}

func TestRunRefusesAnHTTPRepositoryAndSaysWhy(t *testing.T) {
	var stderr bytes.Buffer

	env := environment(map[string]string{config.EnvRepoURL: "http://git.internal/acme/dashboards.git"})

	code := run(context.Background(), env, &stderr)

	if code != exitConfig {
		t.Errorf("exit code %d, want %d: a restart does not make plain http safe", code, exitConfig)
	}
	for _, want := range []string{config.EnvRepoURL, "must not use http://", "NFR-15"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the message does not say %q:\n%s", want, stderr.String())
		}
	}
}
