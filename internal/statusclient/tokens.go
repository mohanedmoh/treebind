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

package statusclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// TokenLifetime is the lifetime that is asked for, which is the least the API
	// server grants.
	TokenLifetime = 10 * time.Minute

	// TokenRenewBefore is how much of a token has to be left for it to be used. A
	// token with less is replaced, so that a call is never made with one that
	// runs out on the way, and the agent's tolerance of a clock that differs is
	// not leaned on.
	TokenRenewBefore = 5 * time.Minute

	// tokenPruneEvery is how often the tokens that ran out are forgotten.
	tokenPruneEvery = time.Minute
)

// ServiceAccountTokens gives tokens of the operator's own service account, with the
// audience that is asked for, and keeps each in memory until it is nearly over. It
// asks the API server for a new one when the agent refuses the one it has
// (Forget). It is safe for concurrent use. An audience is the Treebind whose agent
// is polled, and a Treebind is polled by one caller at a time, so two calls that
// both miss the cache for one audience do not happen in practice. If they do, each
// asks the API server and the later token is the one kept.
//
// The permission it needs is a Role in the operator's own namespace: create on
// serviceaccounts/token, with resourceNames set to the operator's service account.
// A token is a credential for the agent of one Treebind, and nothing else: its
// audience names that Treebind, so the API server refuses it (docs/adr/0011). It is
// held only in memory, and it is never logged.
type ServiceAccountTokens struct {
	// Client makes the requests.
	Client client.Client
	// Namespace and ServiceAccount name the operator's own service account.
	Namespace, ServiceAccount string

	// now is time.Now unless a test replaces it.
	now func() time.Time

	mu       sync.Mutex
	kept     map[string]keptToken
	prunedAt time.Time
}

// keptToken is a token and the time it runs out.
type keptToken struct {
	token   string
	expires time.Time
}

// Token returns a token for the audience, from memory when it has more than
// TokenRenewBefore left, and otherwise from the API server.
func (s *ServiceAccountTokens) Token(ctx context.Context, audience string) (string, error) {
	if audience == "" {
		// A token with no audience is good for the API server itself.
		return "", errors.New("a token is asked for with no audience")
	}
	if token, ok := s.keptToken(audience); ok {
		return token, nil
	}

	lifetime := int64(TokenLifetime / time.Second)
	request := &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
		Audiences:         []string{audience},
		ExpirationSeconds: &lifetime,
	}}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: s.ServiceAccount, Namespace: s.Namespace}}
	if err := s.Client.SubResource("token").Create(ctx, account, request); err != nil {
		return "", fmt.Errorf("request a token of %s/%s with the audience %q: %w", s.Namespace, s.ServiceAccount, audience, err)
	}
	if request.Status.Token == "" {
		return "", fmt.Errorf("the API server sent no token for %s/%s with the audience %q", s.Namespace, s.ServiceAccount, audience)
	}

	expires := request.Status.ExpirationTimestamp.Time
	if expires.IsZero() {
		expires = s.clock().Add(TokenLifetime)
	}
	s.keep(audience, request.Status.Token, expires)
	return request.Status.Token, nil
}

// Forget drops the token kept for the audience, if it is the given one. The agent
// refused that token, and a newer one that was kept meanwhile is not to blame.
func (s *ServiceAccountTokens) Forget(audience, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept, ok := s.kept[audience]; ok && kept.token == token {
		delete(s.kept, audience)
	}
}

func (s *ServiceAccountTokens) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *ServiceAccountTokens) keptToken(audience string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, ok := s.kept[audience]
	if !ok || kept.expires.Sub(s.clock()) <= TokenRenewBefore {
		return "", false
	}
	return kept.token, true
}

// keep remembers a token, and forgets the ones that ran out, so that the tokens of
// Treebinds that no longer exist do not stay for ever.
func (s *ServiceAccountTokens) keep(audience, token string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.kept == nil {
		s.kept = make(map[string]keptToken)
	}
	s.kept[audience] = keptToken{token: token, expires: expires}

	if now.Sub(s.prunedAt) >= tokenPruneEvery {
		s.prunedAt = now
		for key, kept := range s.kept {
			if !kept.expires.After(now) {
				delete(s.kept, key)
			}
		}
	}
}
