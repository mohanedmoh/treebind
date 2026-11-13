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
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testIssuer  = "https://kubernetes.default.svc.cluster.local"
	testSubject = "system:serviceaccount:treebind-system:treebind-controller-manager"
	// anIssuerDocument is a discovery document that names an issuer.
	anIssuerDocument   = `{"issuer":"x"}`
	notAServiceAccount = "not a service account"
)

// serving is an API server that publishes a discovery document.
func serving(document string) rawGetter {
	return func(_ context.Context, path string) ([]byte, error) {
		if path != "/.well-known/openid-configuration" {
			return nil, errors.New("the path " + path + " is not served")
		}
		return []byte(document), nil
	}
}

// neverAsked fails the test if the API server is called.
func neverAsked(t *testing.T) rawGetter {
	t.Helper()
	return func(context.Context, string) ([]byte, error) {
		t.Error("the API server was asked for something that was given")
		return nil, errors.New("not served")
	}
}

func TestTheSettingsAreFoundWhenNothingIsGiven(t *testing.T) {
	got, err := resolveStatusSettings(context.Background(), statusSettings{},
		"treebind-system", "treebind-controller-manager", serving(`{"issuer":"`+testIssuer+`","jwks_uri":"x"}`))

	if err != nil {
		t.Fatal(err)
	}
	if got != (statusSettings{Issuer: testIssuer, Subject: testSubject}) {
		t.Errorf("got %+v", got)
	}
}

func TestWhatIsGivenIsUsedAsItIs(t *testing.T) {
	given := statusSettings{Issuer: "https://issuer.example", Subject: "system:serviceaccount:ops:treebind"}

	got, err := resolveStatusSettings(context.Background(), given, "ignored", "ignored", neverAsked(t))

	if err != nil || got != given {
		t.Errorf("got %+v, %v, want %+v", got, err, given)
	}
}

func TestAGivenIssuerNeedsNoDiscoveryAndAGivenSubjectNeedsNoEnvironment(t *testing.T) {
	got, err := resolveStatusSettings(context.Background(), statusSettings{Issuer: testIssuer}, "ns", "sa", neverAsked(t))
	if err != nil || got.Issuer != testIssuer || got.Subject != "system:serviceaccount:ns:sa" {
		t.Errorf("issuer given: %+v, %v", got, err)
	}

	got, err = resolveStatusSettings(context.Background(), statusSettings{Subject: testSubject}, "", "",
		serving(`{"issuer":"`+testIssuer+`"}`))
	if err != nil || got.Issuer != testIssuer || got.Subject != testSubject {
		t.Errorf("subject given: %+v, %v", got, err)
	}
}

func TestTheOperatorRefusesToGuess(t *testing.T) {
	tests := []struct {
		name      string
		given     statusSettings
		namespace string
		account   string
		document  string
		want      string
	}{
		{"no namespace", statusSettings{}, "", "sa", anIssuerDocument, "POD_NAMESPACE"},
		{"no service account", statusSettings{}, "ns", "", anIssuerDocument, "POD_SERVICE_ACCOUNT"},
		{"a subject that is not a service account", statusSettings{Subject: "admin"}, "", "",
			anIssuerDocument, notAServiceAccount},
		{"a user as the subject", statusSettings{Subject: "system:node:worker"}, "", "",
			anIssuerDocument, notAServiceAccount},
		{"a document that is not JSON", statusSettings{Subject: testSubject}, "", "", `<html>`, "not a discovery document"},
		{"a document with no issuer", statusSettings{Subject: testSubject}, "", "", `{"jwks_uri":"x"}`, "names no issuer"},
		{"an empty issuer", statusSettings{Subject: testSubject}, "", "", `{"issuer":""}`, "names no issuer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveStatusSettings(context.Background(), tt.given, tt.namespace, tt.account, serving(tt.document))

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error that says %q", err, tt.want)
			}
		})
	}
}

func TestAnUnreadableDiscoveryDocumentSaysHowToGoOn(t *testing.T) {
	failing := func(context.Context, string) ([]byte, error) { return nil, errors.New("forbidden") }

	_, err := resolveStatusSettings(context.Background(), statusSettings{Subject: testSubject}, "", "", failing)

	if err == nil || !strings.Contains(err.Error(), "--status-issuer") || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("got %v, want a message that names the flag and the cause", err)
	}
}

func TestDiscoveryDoesNotWaitForEver(t *testing.T) {
	var deadline time.Time
	hung := func(ctx context.Context, _ string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return nil, ctx.Err()
	}

	before := time.Now()
	_, _ = resolveStatusSettings(context.Background(), statusSettings{Subject: testSubject}, "", "", hung)

	if deadline.IsZero() || deadline.Sub(before) > 20*time.Second {
		t.Errorf("the request had the deadline %v, want one within 20 s of %v", deadline, before)
	}
}
