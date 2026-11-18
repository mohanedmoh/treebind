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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// The manager only builds its leader elector when it starts, so timings that break
// client-go's rules would show up as a crash loop of both replicas in a cluster.
// This hands the configured timings to the same constructor, which holds the rules,
// so a change to one of them fails here first.
func TestLeaderElectionTimingsAreAccepted(t *testing.T) {
	const name = "test"
	_, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: name, Namespace: name},
			LockConfig: resourcelock.ResourceLockConfig{Identity: name},
		},
		LeaseDuration: leaderLeaseDuration,
		RenewDeadline: leaderRenewDeadline,
		RetryPeriod:   leaderRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {},
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		t.Fatalf("client-go does not accept the leader election timings (lease %s, renew deadline %s, retry %s): %v",
			leaderLeaseDuration, leaderRenewDeadline, leaderRetryPeriod, err)
	}
}

// docs/operator-replicas.md quotes the timings, and a reader plans around them, so
// the page has to follow the code.
func TestLeaderElectionTimingsAreInTheDocumentation(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "operator-replicas.md"))
	if err != nil {
		t.Fatalf("read the page that documents the timings: %v", err)
	}

	tests := []struct {
		name  string
		value time.Duration
	}{
		{"Lease duration", leaderLeaseDuration},
		{"Renew deadline", leaderRenewDeadline},
		{"Retry period", leaderRetryPeriod},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := fmt.Sprintf("| %s | %d seconds |", tt.name, int(tt.value/time.Second))
			if !strings.Contains(string(page), row) {
				t.Errorf("docs/operator-replicas.md has no table row that starts %q; change it and the timing together", row)
			}
		})
	}
}

// main cannot be run in a test, so this holds the wiring of --watch-namespaces as it reads: the flag is read
// with the parser that checks it, an invalid list stops the operator before it connects to anything, and the
// namespaces reach the cache of the manager. The cluster run of #30 held that it works.
func TestTheFlagForNamespacesIsParsedAndReachesTheCacheOfTheManager(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, want := range []string{
		`flag.StringVar(&watchNamespaces, "watch-namespaces"`,
		`controller.ParseWatchNamespaces(watchNamespaces)`,
		`Invalid --watch-namespaces`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("main.go does not hold %q", want)
		}
	}
	if !regexp.MustCompile(`Cache:\s+controller\.CacheOptions\(namespaces\),`).MatchString(text) {
		t.Error("main.go does not give the manager the cache options that the namespaces make")
	}
	// The list is read and checked before the operator connects to the API server, so that a bad one fails at once.
	if strings.Index(text, "ParseWatchNamespaces") > strings.Index(text, "ctrl.GetConfigOrDie()") {
		t.Error("the list of namespaces is read after the operator has connected to the API server")
	}
}
