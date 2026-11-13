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
	"strings"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	operatorNamespace = "treebind-system"
	operatorAccount   = "treebind-controller-manager"
)

// fakeClock is a clock that only moves when a test moves it.
type fakeClock struct{ at time.Time }

func newFakeClock() *fakeClock { return &fakeClock{at: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time          { return c.at }
func (c *fakeClock) Advance(d time.Duration) { c.at = c.at.Add(d) }

// tokenAPI plays the TokenRequest endpoint of the API server and keeps what it is
// asked.
type tokenAPI struct {
	clock *fakeClock

	mu       sync.Mutex
	requests []tokenAsked
	// lifetime is how long the API server grants a token. Zero grants what is asked.
	lifetime time.Duration
	// noExpiry leaves the expiry out of the answer.
	noExpiry bool
	// empty answers with no token.
	empty bool
	err   error
}

type tokenAsked struct {
	account   accountRef
	audiences []string
	seconds   *int64
}

// accountRef is the namespace and name of the service account a token is asked for.
type accountRef struct{ namespace, name string }

func (a *tokenAPI) asked() []tokenAsked {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]tokenAsked(nil), a.requests...)
}

func (a *tokenAPI) create(_ context.Context, _ client.Client, subResource string, obj client.Object,
	body client.Object, _ ...client.SubResourceCreateOption,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if subResource != "token" {
		return fmt.Errorf("unexpected subresource %q", subResource)
	}
	request := body.(*authv1.TokenRequest)
	a.requests = append(a.requests, tokenAsked{
		account:   accountRef{obj.GetNamespace(), obj.GetName()},
		audiences: request.Spec.Audiences,
		seconds:   request.Spec.ExpirationSeconds,
	})
	if a.err != nil {
		return a.err
	}
	granted := a.lifetime
	if granted == 0 {
		granted = time.Duration(*request.Spec.ExpirationSeconds) * time.Second
	}
	if !a.empty {
		request.Status.Token = fmt.Sprintf("token-%d-for-%s", len(a.requests), request.Spec.Audiences[0])
	}
	if !a.noExpiry {
		request.Status.ExpirationTimestamp = metav1.NewTime(a.clock.Now().Add(granted))
	}
	return nil
}

func newTestTokens(api *tokenAPI, clock *fakeClock) *ServiceAccountTokens {
	api.clock = clock
	tokens := &ServiceAccountTokens{
		Client: fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{SubResourceCreate: api.create}).Build(),
		Namespace:      operatorNamespace,
		ServiceAccount: operatorAccount,
	}
	tokens.now = clock.Now
	return tokens
}

func TestTokensAreAskedForTheOperatorsOwnServiceAccount(t *testing.T) {
	api := &tokenAPI{}
	tokens := newTestTokens(api, newFakeClock())

	token, err := tokens.Token(context.Background(), testAudience)
	if err != nil {
		t.Fatal(err)
	}

	asked := api.asked()
	if len(asked) != 1 {
		t.Fatalf("%d requests, want 1", len(asked))
	}
	if asked[0].account != (accountRef{operatorNamespace, operatorAccount}) {
		t.Errorf("the token was asked for the service account %+v, want the operator's own", asked[0].account)
	}
	if len(asked[0].audiences) != 1 || asked[0].audiences[0] != testAudience {
		t.Errorf("audiences = %v, want only %s: a token with another audience is worth less", asked[0].audiences, testAudience)
	}
	if asked[0].seconds == nil || *asked[0].seconds != 600 {
		t.Errorf("lifetime = %v seconds, want 600, the least the API server grants", asked[0].seconds)
	}
	if token != "token-1-for-"+testAudience {
		t.Errorf("token = %q, want the one the API server gave", token)
	}
}

func TestATokenIsKeptUntilItIsNearlyOver(t *testing.T) {
	api, clock := &tokenAPI{}, newFakeClock()
	tokens := newTestTokens(api, clock)
	ctx := context.Background()
	first, err := tokens.Token(ctx, testAudience)
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(TokenLifetime - TokenRenewBefore - time.Second)
	again, err := tokens.Token(ctx, testAudience)
	if err != nil || again != first || len(api.asked()) != 1 {
		t.Errorf("just before the renewal: %q, %v after %d requests, want the first token after 1", again, err, len(api.asked()))
	}

	clock.Advance(time.Second)
	renewed, err := tokens.Token(ctx, testAudience)
	if err != nil || renewed == first || len(api.asked()) != 2 {
		t.Errorf("with %v left: %q, %v after %d requests, want a new token after 2", TokenRenewBefore, renewed, err, len(api.asked()))
	}
}

func TestATokenIsKeptForAsLongAsTheAPIServerGrantedIt(t *testing.T) {
	api, clock := &tokenAPI{lifetime: 15 * time.Minute}, newFakeClock()
	tokens := newTestTokens(api, clock)
	ctx := context.Background()
	if _, err := tokens.Token(ctx, testAudience); err != nil {
		t.Fatal(err)
	}

	clock.Advance(10*time.Minute - time.Second) // 5 minutes and a second left of the 15 granted
	if _, err := tokens.Token(ctx, testAudience); err != nil || len(api.asked()) != 1 {
		t.Errorf("a token with more than %v left was asked for again: %v after %d requests", TokenRenewBefore, err, len(api.asked()))
	}
	clock.Advance(time.Second)
	if _, err := tokens.Token(ctx, testAudience); err != nil || len(api.asked()) != 2 {
		t.Errorf("a token with %v left was kept: %v after %d requests", TokenRenewBefore, err, len(api.asked()))
	}
}

func TestATokenWithNoExpiryIsKeptForTheLifetimeThatWasAsked(t *testing.T) {
	api, clock := &tokenAPI{noExpiry: true}, newFakeClock()
	tokens := newTestTokens(api, clock)
	ctx := context.Background()
	if _, err := tokens.Token(ctx, testAudience); err != nil {
		t.Fatal(err)
	}

	clock.Advance(TokenLifetime - TokenRenewBefore - time.Second)
	if _, err := tokens.Token(ctx, testAudience); err != nil || len(api.asked()) != 1 {
		t.Errorf("%v after %d requests, want the token kept", err, len(api.asked()))
	}
	clock.Advance(time.Second)
	if _, err := tokens.Token(ctx, testAudience); err != nil || len(api.asked()) != 2 {
		t.Errorf("%v after %d requests, want a new token", err, len(api.asked()))
	}
}

func TestEveryAudienceHasItsOwnToken(t *testing.T) {
	api := &tokenAPI{}
	tokens := newTestTokens(api, newFakeClock())
	ctx := context.Background()

	one, _ := tokens.Token(ctx, "treebind.example.org/apps/one")
	two, _ := tokens.Token(ctx, "treebind.example.org/apps/two")
	oneAgain, _ := tokens.Token(ctx, "treebind.example.org/apps/one")

	if one == two || !strings.HasSuffix(one, "/one") || !strings.HasSuffix(two, "/two") {
		t.Errorf("tokens = %q and %q, want one for each audience", one, two)
	}
	if oneAgain != one || len(api.asked()) != 2 {
		t.Errorf("the first audience got %q after %d requests, want its own token kept", oneAgain, len(api.asked()))
	}
}

func TestForgettingATokenAsksForANewOneOnlyForThatAudience(t *testing.T) {
	api := &tokenAPI{}
	tokens := newTestTokens(api, newFakeClock())
	ctx := context.Background()
	first, _ := tokens.Token(ctx, "treebind.example.org/apps/one")
	other, _ := tokens.Token(ctx, "treebind.example.org/apps/two")

	tokens.Forget("treebind.example.org/apps/one", first)

	renewed, _ := tokens.Token(ctx, "treebind.example.org/apps/one")
	kept, _ := tokens.Token(ctx, "treebind.example.org/apps/two")
	if renewed == first {
		t.Error("the token that was forgotten came back")
	}
	if kept != other {
		t.Error("the token of another audience was forgotten with it")
	}
	tokens.Forget("treebind.example.org/apps/never-asked", "no token") // must not panic
}

func TestForgettingAFormerTokenKeepsTheCurrentOne(t *testing.T) {
	api := &tokenAPI{}
	tokens := newTestTokens(api, newFakeClock())
	ctx := context.Background()
	current, _ := tokens.Token(ctx, testAudience)

	// The agent refused an earlier token, which says nothing about this one.
	tokens.Forget(testAudience, "an-earlier-token")

	kept, err := tokens.Token(ctx, testAudience)
	if err != nil || kept != current || len(api.asked()) != 1 {
		t.Errorf("the token is %q (%v) after %d requests, want the current one kept", kept, err, len(api.asked()))
	}
}

func TestAFailedRequestIsReportedAndNotKept(t *testing.T) {
	cause := errors.New("serviceaccounts \"treebind-controller-manager\" is forbidden")
	api, clock := &tokenAPI{err: cause}, newFakeClock()
	tokens := newTestTokens(api, clock)
	ctx := context.Background()

	_, err := tokens.Token(ctx, testAudience)

	if !errors.Is(err, cause) || !strings.Contains(err.Error(), testAudience) {
		t.Errorf("error = %v, want the cause, wrapped, with the audience it was for", err)
	}
	api.mu.Lock()
	api.err = nil
	api.mu.Unlock()
	if _, err := tokens.Token(ctx, testAudience); err != nil || len(api.asked()) != 2 {
		t.Errorf("after the failure: %v after %d requests, want a second request that works", err, len(api.asked()))
	}
}

func TestAnAnswerWithNoTokenIsAnError(t *testing.T) {
	tokens := newTestTokens(&tokenAPI{empty: true}, newFakeClock())

	token, err := tokens.Token(context.Background(), testAudience)

	if err == nil || token != "" || !strings.Contains(err.Error(), "no token") {
		t.Errorf("Token = %q, %v, want an error that says the API server sent no token", token, err)
	}
}

func TestNoTokenIsAskedForWithoutAnAudience(t *testing.T) {
	api := &tokenAPI{}
	tokens := newTestTokens(api, newFakeClock())

	if _, err := tokens.Token(context.Background(), ""); err == nil {
		t.Error("a token with no audience was asked for: it would be good for the API server itself")
	}
	if len(api.asked()) != 0 {
		t.Errorf("%d requests were made", len(api.asked()))
	}
}

func TestTokensThatRanOutAreForgottenWhenAnotherIsKept(t *testing.T) {
	api, clock := &tokenAPI{}, newFakeClock()
	tokens := newTestTokens(api, clock)
	ctx := context.Background()
	if _, err := tokens.Token(ctx, "treebind.example.org/apps/gone"); err != nil {
		t.Fatal(err)
	}

	clock.Advance(TokenLifetime - time.Second) // not over, but past the renewal
	if _, err := tokens.Token(ctx, "treebind.example.org/apps/new"); err != nil {
		t.Fatal(err)
	}
	if _, kept := tokens.kept["treebind.example.org/apps/gone"]; !kept {
		t.Error("a token that has not run out was forgotten")
	}

	clock.Advance(tokenPruneEvery)
	if _, err := tokens.Token(ctx, "treebind.example.org/apps/newer"); err != nil {
		t.Fatal(err)
	}
	if _, kept := tokens.kept["treebind.example.org/apps/gone"]; kept {
		t.Error("a token that ran out is still kept, so the tokens of deleted Treebinds pile up")
	}
	if _, kept := tokens.kept["treebind.example.org/apps/newer"]; !kept {
		t.Error("the token that was just asked for was forgotten")
	}
}

func TestTokensAreSafeToAskForFromManyGoroutines(t *testing.T) {
	tokens := newTestTokens(&tokenAPI{}, newFakeClock())

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			audience := fmt.Sprintf("treebind.example.org/apps/%d", i%8)
			if _, err := tokens.Token(context.Background(), audience); err != nil {
				t.Error(err)
			}
			if i%5 == 0 {
				tokens.Forget(audience, "a-token")
			}
		})
	}
	wg.Wait()
}

func TestTokensCarryTheNamespaceOfTheOperatorAndNotOfTheTreebind(t *testing.T) {
	// The token is for the operator's service account, which lives in the operator's
	// namespace, whichever namespace the Treebind is in.
	api := &tokenAPI{}
	tokens := newTestTokens(api, newFakeClock())

	if _, err := tokens.Token(context.Background(), "treebind.example.org/elsewhere/x"); err != nil {
		t.Fatal(err)
	}
	if got := api.asked()[0].account.namespace; got != operatorNamespace {
		t.Errorf("namespace = %s, want %s", got, operatorNamespace)
	}
}
