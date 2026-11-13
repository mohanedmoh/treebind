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
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSigningKey is generated once, because generating an RSA key is slow.
var testSigningKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

const testKeyID = "key-1"

// jwksOf returns a key set the way the API server publishes it, with the public
// half of key.
func jwksOf(kid string, key *rsa.PublicKey) []byte {
	encode := func(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }
	data, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
		"n": encode(key.N.Bytes()), "e": encode(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if err != nil {
		panic(err)
	}
	return data
}

// testKeySet is the key set of testSigningKey under testKeyID.
var testKeySet = sync.OnceValue(func() []byte { return jwksOf(testKeyID, &testSigningKey().PublicKey) })

// fakeClock is a clock that only moves when a test moves it.
type fakeClock struct{ at time.Time }

func newFakeClock() *fakeClock { return &fakeClock{at: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time          { return c.at }
func (c *fakeClock) Advance(d time.Duration) { c.at = c.at.Add(d) }

// fakeAPI answers reads of the API server's paths and counts them.
type fakeAPI struct {
	calls atomic.Int32
	// answer is what the next read returns.
	mu       sync.Mutex
	body     []byte
	err      error
	lastPath string
	deadline bool
}

func (a *fakeAPI) set(body []byte, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.body, a.err = body, err
}

func (a *fakeAPI) get(ctx context.Context, path string) ([]byte, error) {
	a.calls.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastPath = path
	_, a.deadline = ctx.Deadline()
	return a.body, a.err
}

func newTestFetcher(api *fakeAPI, clock *fakeClock) *keyFetcher {
	fetcher := newKeyFetcher(api.get)
	fetcher.now = clock.Now
	return fetcher
}

func TestKeyFetcherReadsTheKeySetFromTheAPIServer(t *testing.T) {
	keys := jwksOf(testKeyID, &testSigningKey().PublicKey)
	api := &fakeAPI{}
	api.set(keys, nil)

	got, err := newTestFetcher(api, newFakeClock()).Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(keys) {
		t.Errorf("keys = %s, want what the API server sent, %s", got, keys)
	}
	if api.lastPath != "/openid/v1/jwks" {
		t.Errorf("read %q, want /openid/v1/jwks", api.lastPath)
	}
	if !api.deadline {
		t.Error("the read has no time limit, so a hung API server would hold the reconcile")
	}
}

func TestKeyFetcherReadsAgainOnlyOnceTheCopyIsOld(t *testing.T) {
	first := jwksOf("first", &testSigningKey().PublicKey)
	second := jwksOf("second", &testSigningKey().PublicKey)
	api, clock := &fakeAPI{}, newFakeClock()
	api.set(first, nil)
	fetcher := newTestFetcher(api, clock)
	ctx := context.Background()

	if _, err := fetcher.Keys(ctx); err != nil {
		t.Fatal(err)
	}
	api.set(second, nil)

	clock.Advance(keysFetchTTL - time.Second)
	got, err := fetcher.Keys(ctx)
	if err != nil || string(got) != string(first) || api.calls.Load() != 1 {
		t.Errorf("just under the limit: %s, %v after %d reads, want the first copy after 1", got, err, api.calls.Load())
	}

	clock.Advance(time.Second)
	got, err = fetcher.Keys(ctx)
	if err != nil || string(got) != string(second) || api.calls.Load() != 2 {
		t.Errorf("at the limit: %s, %v after %d reads, want the second copy after 2", got, err, api.calls.Load())
	}
}

func TestKeyFetcherSharesOneReadBetweenCallersAtTheSameTime(t *testing.T) {
	api := &fakeAPI{}
	api.set(jwksOf(testKeyID, &testSigningKey().PublicKey), nil)
	fetcher := newTestFetcher(api, newFakeClock())

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := fetcher.Keys(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if calls := api.calls.Load(); calls != 1 {
		t.Errorf("%d reads for 20 callers at once, want 1", calls)
	}
}

func TestKeyFetcherKeepsTheLastGoodCopyWhenARefreshFailsButNotForEver(t *testing.T) {
	keys := jwksOf(testKeyID, &testSigningKey().PublicKey)
	api, clock := &fakeAPI{}, newFakeClock()
	api.set(keys, nil)
	fetcher := newTestFetcher(api, clock)
	ctx := context.Background()
	if _, err := fetcher.Keys(ctx); err != nil {
		t.Fatal(err)
	}

	errDown := errors.New("the API server is down")
	api.set(nil, errDown)

	clock.Advance(keysMaxStale - time.Second)
	got, err := fetcher.Keys(ctx)
	if err != nil || string(got) != string(keys) {
		t.Errorf("just under the stale limit: %s, %v, want the last good copy", got, err)
	}

	clock.Advance(time.Second)
	if _, err = fetcher.Keys(ctx); !errors.Is(err, errDown) || !strings.Contains(err.Error(), "read /openid/v1/jwks") {
		t.Errorf("at the stale limit: error = %v, want the reason of the failed read, wrapped", err)
	}
}

func TestKeyFetcherDoesNotReadAgainRightAfterAFailure(t *testing.T) {
	api, clock := &fakeAPI{}, newFakeClock()
	api.set(nil, errors.New("forbidden"))
	fetcher := newTestFetcher(api, clock)
	ctx := context.Background()

	for range 5 {
		if _, err := fetcher.Keys(ctx); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("error = %v, want the reason of the failed read", err)
		}
	}
	if calls := api.calls.Load(); calls != 1 {
		t.Errorf("%d reads for 5 calls, want 1: a Treebind each would read again and log again", calls)
	}

	clock.Advance(keysFetchTTL)
	api.set(jwksOf(testKeyID, &testSigningKey().PublicKey), nil)
	if _, err := fetcher.Keys(ctx); err != nil {
		t.Errorf("after the wait: %v, want the read to be tried again and to work", err)
	}
}

func TestKeyFetcherNeverPublishesWhatTheAgentCannotUse(t *testing.T) {
	good := jwksOf(testKeyID, &testSigningKey().PublicKey)
	for name, bad := range map[string][]byte{
		"a page that is not a key set": []byte("<html>unauthorized</html>"),
		"a key set with no key":        []byte(`{"keys":[]}`),
	} {
		t.Run(name+" at the first read", func(t *testing.T) {
			api := &fakeAPI{}
			api.set(bad, nil)
			_, err := newTestFetcher(api, newFakeClock()).Keys(context.Background())
			if err == nil || !strings.Contains(err.Error(), "sync agents cannot use") {
				t.Errorf("error = %v, want it to say the agents cannot use it", err)
			}
		})
		t.Run(name+" after good keys", func(t *testing.T) {
			api, clock := &fakeAPI{}, newFakeClock()
			api.set(good, nil)
			fetcher := newTestFetcher(api, clock)
			if _, err := fetcher.Keys(context.Background()); err != nil {
				t.Fatal(err)
			}
			api.set(bad, nil)
			clock.Advance(keysFetchTTL)
			got, err := fetcher.Keys(context.Background())
			if err != nil || string(got) != string(good) {
				t.Errorf("keys = %s, %v, want the good copy to stay", got, err)
			}
		})
	}
}

func TestKeyFetcherIsNotHeldBackByACancelledCaller(t *testing.T) {
	api := &fakeAPI{}
	api.set(jwksOf(testKeyID, &testSigningKey().PublicKey), nil)
	fetcher := newTestFetcher(api, newFakeClock())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	api.set(nil, context.Canceled)
	if _, err := fetcher.Keys(cancelled); err == nil {
		t.Fatal("the read of a cancelled caller worked")
	}

	// The caller gave up, which says nothing about the API server, so the next
	// caller must not be told to wait for the failure to age.
	api.set(jwksOf(testKeyID, &testSigningKey().PublicKey), nil)
	if _, err := fetcher.Keys(context.Background()); err != nil {
		t.Errorf("the next caller: %v, want a read that works", err)
	}
	if calls := api.calls.Load(); calls != 2 {
		t.Errorf("%d reads, want 2", calls)
	}
}
