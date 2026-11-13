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
	"fmt"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/mohanedmoh/treebind/internal/agent/auth"
)

const (
	// jwksPath is where the API server publishes the public keys that sign the
	// tokens it issues. Every service account may read it, so the operator needs no
	// permission of its own for it.
	jwksPath = "/openid/v1/jwks"

	// keysFetchTTL is how long a read of the keys, or a failure to read them, is
	// used before the API server is asked again. One read serves every Treebind.
	keysFetchTTL = time.Minute
	// keysMaxStale is how long the last good copy is still handed out when the
	// reads fail. It is the time the agents themselves trust keys they cannot
	// refresh (docs/adr/0011), and keys change only when the cluster rotates them.
	keysMaxStale = time.Hour
	// keysFetchTimeout bounds one read, so that an API server that does not answer
	// does not hold a reconcile.
	keysFetchTimeout = 15 * time.Second
)

// KeySource returns the cluster's public signing keys, as the JSON Web Key Set the
// API server publishes. The sync agents check the operator's token against them.
type KeySource interface {
	Keys(ctx context.Context) ([]byte, error)
}

// APIGetter reads a path of the API server.
type APIGetter func(ctx context.Context, path string) ([]byte, error)

// keyFetcher reads the key set from the API server for every Treebind at once. It
// keeps the last copy that a sync agent can use, and never hands out anything else:
// a page that is not a key set must not replace keys that work.
type keyFetcher struct {
	get APIGetter
	now func() time.Time

	// mu is held during a read, so that callers who arrive meanwhile wait for it
	// and use its result instead of reading again.
	mu          sync.Mutex
	keys        []byte
	fetchedAt   time.Time
	attemptedAt time.Time
	lastErr     error
}

func newKeyFetcher(get APIGetter) *keyFetcher {
	return &keyFetcher{get: get, now: time.Now}
}

// NewKeyFetcher returns a KeySource that reads the keys through get, at most once
// a minute. When a read fails it hands out the last good copy for up to an hour,
// and the error after that.
func NewKeyFetcher(get APIGetter) KeySource {
	return newKeyFetcher(get)
}

// Keys returns the current key set.
func (f *keyFetcher) Keys(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	if f.attemptedAt.IsZero() || now.Sub(f.attemptedAt) >= keysFetchTTL {
		f.attemptedAt = now
		keys, err := f.fetch(ctx)
		switch {
		case err == nil:
			f.keys, f.fetchedAt, f.lastErr = keys, now, nil
		case ctx.Err() != nil:
			// The caller gave up, which says nothing about the API server. The next
			// caller must not be told that the failure has to age first.
			f.attemptedAt = time.Time{}
			return nil, err
		default:
			f.lastErr = err
			logf.FromContext(ctx).Error(err, "Could not read the cluster's public signing keys",
				"haveCopy", f.keys != nil)
		}
	}

	if f.keys != nil && now.Sub(f.fetchedAt) < keysMaxStale {
		return f.keys, nil
	}
	return nil, f.lastErr
}

func (f *keyFetcher) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, keysFetchTimeout)
	defer cancel()

	keys, err := f.get(ctx, jwksPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", jwksPath, err)
	}
	if err := auth.CheckKeys(keys); err != nil {
		return nil, fmt.Errorf("the key set at %s is one the sync agents cannot use: %w", jwksPath, err)
	}
	return keys, nil
}
