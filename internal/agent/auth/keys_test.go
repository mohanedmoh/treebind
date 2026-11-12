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

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseKeys(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	// changed returns an entry with a change made to it.
	changed := func(entry jwk, change func(*jwk)) jwk {
		change(&entry)
		return entry
	}
	good := rsaJWK(kidRSA, &keys().rsa.PublicKey)
	goodEC := ecJWK(&keys().ec.PublicKey)

	t.Run("takes RSA and EC signing keys", func(t *testing.T) {
		set, err := parseKeys(keyFile(t, good, goodEC), now)
		if err != nil || len(set.byKID) != 2 {
			t.Fatalf("%v, %v", set, err)
		}
	})

	skipped := map[string]jwk{
		"an RSA key of 1024 bits":              rsaJWK("small", &small.PublicKey),
		"an RSA key of more than 8192 bits":    changed(good, func(k *jwk) { k.N = b64(append([]byte{0x80}, make([]byte, 1100)...)) }),
		"a key with no ID":                     changed(good, func(k *jwk) { k.Kid = "" }),
		"a key for encryption":                 changed(good, func(k *jwk) { k.Use = "enc" }),
		"an RSA key that names another alg":    changed(good, func(k *jwk) { k.Alg = "RS384" }),
		"an RSA key that names the EC alg":     changed(good, func(k *jwk) { k.Alg = algES256 }),
		"an EC key that names the RSA alg":     changed(goodEC, func(k *jwk) { k.Alg = algRS256 }),
		"an EC key on another curve":           changed(goodEC, func(k *jwk) { k.Crv = "P-384" }),
		"an EC point that is not on the curve": changed(goodEC, func(k *jwk) { k.Y = goodEC.X }),
		"an EC coordinate that is too short":   changed(goodEC, func(k *jwk) { k.X = "AQ" }),
		"an RSA exponent of 1":                 changed(good, func(k *jwk) { k.E = "AQ" }),
		"an even RSA exponent":                 changed(good, func(k *jwk) { k.E = "AA" }),
		"an RSA exponent that is huge":         changed(good, func(k *jwk) { k.E = "_____w" }),
		"an RSA modulus that is not base64":    changed(good, func(k *jwk) { k.N = "***" }),
		"a key type this package does not use": {Kty: "OKP", Kid: "ed", Crv: "Ed25519", X: "AAAA"},
	}
	for name, entry := range skipped {
		t.Run("skips "+name+", and keeps the good key", func(t *testing.T) {
			set, err := parseKeys(keyFile(t, entry, good), now)
			if err != nil {
				t.Fatal(err)
			}
			if len(set.byKID) != 1 || set.byKID[kidRSA] == nil {
				t.Errorf("the key set has %d keys: %v", len(set.byKID), set.byKID)
			}
		})
	}

	t.Run("takes the same key twice as one", func(t *testing.T) {
		set, err := parseKeys(keyFile(t, good, good, goodEC), now)
		if err != nil || len(set.byKID) != 2 {
			t.Errorf("%v, %v", set, err)
		}
	})

	failures := map[string][]byte{
		"a file that is not JSON":          []byte("not json"),
		"a file with no keys":              []byte(`{"keys":[]}`),
		"a file with only keys it skips":   keyFile(t, rsaJWK("small", &small.PublicKey)),
		"two keys under one key ID":        keyFile(t, good, changed(goodEC, func(k *jwk) { k.Kid = kidRSA })),
		"a file larger than the limit":     []byte(strings.Repeat(" ", maxKeysFileBytes+1)),
		"a list where an object is wanted": []byte(`[]`),
	}
	for name, data := range failures {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := parseKeys(data, now); err == nil {
				t.Error("the file was accepted")
			}
		})
	}
}

func TestKeysComeFromTheFile(t *testing.T) {
	t.Run("tokens are refused until the file is there, and then accepted", func(t *testing.T) {
		f := newFixture(t, nil)
		token := rsaToken(t, goodClaims())
		wantError(t, f.Verify(token), errNoKeysLoaded)

		f.setFile(defaultKeyFile(t))
		f.advance(DefaultMinReload)

		if err := f.Verify(token); err != nil {
			t.Errorf("refused once the file is there: %v", err)
		}
	})

	t.Run("a file that appears just after a failed read is used within a second", func(t *testing.T) {
		f := newFixture(t, nil)
		token := rsaToken(t, goodClaims())
		wantError(t, f.Verify(token), errNoKeysLoaded)

		f.setFile(defaultKeyFile(t))
		f.advance(emptyRetry / 2)
		wantError(t, f.Verify(token), errNoKeysLoaded)
		if reads := f.reads.Load(); reads != 1 {
			t.Errorf("the file was read %d times within half a second, want 1", reads)
		}

		f.advance(emptyRetry)
		if err := f.Verify(token); err != nil {
			t.Errorf("not used a second after it appeared: %v", err)
		}
	})

	t.Run("a read that hangs is given up on, and the keys are read again once it returns", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		f.readLimit = 50 * time.Millisecond
		token := rsaToken(t, goodClaims())
		release := make(chan struct{})
		var started atomic.Int64
		good := f.readFile
		f.readFile = func(path string) ([]byte, error) {
			started.Add(1)
			<-release
			return good(path)
		}

		wantError(t, f.Verify(token), errNoKeysLoaded)

		// While the read has not returned, no other read is started.
		f.advance(emptyRetry)
		wantError(t, f.Verify(token), errNoKeysLoaded)
		if n := started.Load(); n != 1 {
			t.Errorf("%d reads were started while the first had not returned, want 1", n)
		}

		close(release)
		deadline := time.Now().Add(5 * time.Second)
		for f.reading.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		f.advance(emptyRetry)

		if err := f.Verify(token); err != nil {
			t.Errorf("the verifier did not recover after the read returned: %v", err)
		}
	})

	t.Run("a key that appears in the file is picked up when a token names it", func(t *testing.T) {
		f := newFixture(t, keyFile(t, ecJWK(&keys().ec.PublicKey)))
		token := rsaToken(t, goodClaims())
		wantError(t, f.Verify(token), errUnknownKey)

		f.setFile(defaultKeyFile(t))
		f.advance(DefaultMinReload)

		if err := f.Verify(token); err != nil {
			t.Errorf("a rotated-in key was not picked up: %v", err)
		}
	})

	t.Run("a file that is damaged keeps the keys that were loaded", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		token := rsaToken(t, goodClaims())
		if err := f.Verify(token); err != nil {
			t.Fatal(err)
		}

		f.setFile([]byte("damaged"))
		f.advance(DefaultMaxKeyAge + time.Second)

		if err := f.Verify(token); err != nil {
			t.Errorf("the operator was shut out by a damaged file: %v", err)
		}
		if f.reads.Load() != 2 {
			t.Errorf("the file was read %d times, want 2", f.reads.Load())
		}
	})

	t.Run("keys that cannot be refreshed are dropped after the maximum staleness", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		claims := goodClaims()
		claims[claimExp] = now.Add(48 * time.Hour).Unix()
		long := rsaToken(t, claims)
		if err := f.Verify(long); err != nil {
			t.Fatal(err)
		}

		f.setFile([]byte("damaged"))
		f.advance(DefaultMaxKeyStale - time.Minute)
		if err := f.Verify(long); err != nil {
			t.Errorf("the keys were dropped before the maximum staleness: %v", err)
		}
		f.advance(2 * time.Minute)

		wantError(t, f.Verify(long), errNoKeysLoaded)

		f.setFile(defaultKeyFile(t))
		f.advance(DefaultMinReload)
		if err := f.Verify(long); err != nil {
			t.Errorf("not trusted again once the file is good: %v", err)
		}
	})

	t.Run("stale keys are not used even when no read is tried", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		f.minReload = 1000 * time.Hour // so that no read is tried
		claims := goodClaims()
		claims[claimExp] = now.Add(48 * time.Hour).Unix()
		long := rsaToken(t, claims)
		if err := f.Verify(long); err != nil {
			t.Fatal(err)
		}

		f.advance(2 * DefaultMaxKeyStale)

		wantError(t, f.Verify(long), errNoKeysLoaded)
		if f.reads.Load() != 1 {
			t.Errorf("a read was tried: %d", f.reads.Load())
		}
	})

	t.Run("a file that cannot be read keeps the keys that were loaded", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		token := rsaToken(t, goodClaims())
		if err := f.Verify(token); err != nil {
			t.Fatal(err)
		}

		f.setFile(nil)
		f.advance(DefaultMaxKeyAge + time.Second)

		if err := f.Verify(token); err != nil {
			t.Errorf("the operator was shut out by an unreadable file: %v", err)
		}
	})

	t.Run("a key taken out of the file stops being trusted after the maximum age", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		token := rsaToken(t, goodClaims())
		if err := f.Verify(token); err != nil {
			t.Fatal(err)
		}

		f.setFile(keyFile(t, ecJWK(&keys().ec.PublicKey)))
		f.advance(DefaultMaxKeyAge - time.Second)
		if err := f.Verify(token); err != nil {
			t.Errorf("the key stopped being trusted before the maximum age: %v", err)
		}
		f.advance(2 * time.Second)

		wantError(t, f.Verify(token), errUnknownKey)
	})
}

func TestTheKeyFileIsNotReadOnEveryRequest(t *testing.T) {
	t.Run("many tokens that name keys that do not exist read it once in the interval", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		for i := range 200 {
			token := signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: "unknown-" + string(rune('a'+i%26))}, goodClaims())
			wantError(t, f.Verify(token), errUnknownKey)
		}

		if reads := f.reads.Load(); reads != 1 {
			t.Errorf("the file was read %d times for 200 refusals, want 1", reads)
		}

		f.advance(DefaultMinReload)
		wantError(t, f.Verify(signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: "again"}, goodClaims())), errUnknownKey)
		if reads := f.reads.Load(); reads != 2 {
			t.Errorf("the file was read %d times after the interval, want 2", reads)
		}
	})

	t.Run("good tokens read it once for the whole period of the keys", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		token := rsaToken(t, goodClaims())
		for range 100 {
			if err := f.Verify(token); err != nil {
				t.Fatal(err)
			}
		}
		if reads := f.reads.Load(); reads != 1 {
			t.Errorf("the file was read %d times for 100 good tokens, want 1", reads)
		}
	})

	t.Run("a flood from several goroutines still reads it once", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		var wg sync.WaitGroup
		var refused atomic.Int64
		for g := range 16 {
			token := signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: "flood-" + string(rune('a'+g))}, goodClaims())
			wg.Go(func() {
				for range 50 {
					if f.Verify(token) != nil {
						refused.Add(1)
					}
				}
			})
		}
		wg.Wait()

		if refused.Load() != 16*50 {
			t.Errorf("%d tokens were refused, want all %d", refused.Load(), 16*50)
		}
		if reads := f.reads.Load(); reads != 1 {
			t.Errorf("the file was read %d times by the flood, want 1", reads)
		}
	})

	t.Run("the first requests, all at once, wait for the keys instead of being refused", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		token := rsaToken(t, goodClaims())

		// The first read is held until the other requests have had time to reach
		// the lock, so that they really meet it.
		entered, release := make(chan struct{}), make(chan struct{})
		read := f.readFile
		var once sync.Once
		f.readFile = func(path string) ([]byte, error) {
			once.Do(func() {
				close(entered)
				<-release
			})
			return read(path)
		}

		var wg sync.WaitGroup
		wg.Go(func() { _ = f.Verify(token) })
		<-entered
		for range 15 {
			wg.Go(func() {
				if err := f.Verify(token); err != nil {
					t.Errorf("a request that arrived while the keys were loading was refused: %v", err)
				}
			})
		}
		time.Sleep(100 * time.Millisecond)
		close(release)
		wg.Wait()

		if reads := f.reads.Load(); reads != 1 {
			t.Errorf("the file was read %d times, want 1", reads)
		}
	})

	t.Run("a token with a new key waits for a read that is under way and is then accepted", func(t *testing.T) {
		f := newFixture(t, keyFile(t, ecJWK(&keys().ec.PublicKey)))
		token := rsaToken(t, goodClaims())
		ecToken := signES256(t, keys().ec, map[string]any{headAlg: algES256, headKid: kidEC}, goodClaims())
		if err := f.Verify(ecToken); err != nil {
			t.Fatal(err)
		}
		f.setFile(defaultKeyFile(t))
		f.advance(DefaultMinReload)

		entered, release := make(chan struct{}), make(chan struct{})
		read := f.readFile
		var once sync.Once
		f.readFile = func(path string) ([]byte, error) {
			once.Do(func() {
				close(entered)
				<-release
			})
			return read(path)
		}

		var wg sync.WaitGroup
		wg.Go(func() { _ = f.Verify(token) })
		<-entered
		wg.Go(func() {
			if err := f.Verify(token); err != nil {
				t.Errorf("a token with a key that is being loaded was refused: %v", err)
			}
		})
		time.Sleep(100 * time.Millisecond)
		close(release)
		wg.Wait()
	})

	t.Run("requests do not wait for a read that hangs", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		f.wait = 50 * time.Millisecond
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		f.readFile = func(string) ([]byte, error) {
			close(entered)
			<-release
			return nil, os.ErrNotExist
		}
		go func() { _ = f.Verify(rsaToken(t, goodClaims())) }()
		<-entered

		result := make(chan error, 1)
		go func() { result <- f.Verify(rsaToken(t, goodClaims())) }()

		select {
		case err := <-result:
			wantError(t, err, errNoKeysLoaded)
		case <-time.After(5 * time.Second):
			t.Fatal("a request kept waiting for a read that hangs")
		}
	})

	t.Run("tokens are verified while the keys are read again, as the race detector sees", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		claims := goodClaims()
		claims[claimExp] = now.Add(24 * time.Hour).Unix()
		token := rsaToken(t, claims)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for range 100 {
					if err := f.Verify(token); err != nil {
						t.Error(err)
						return
					}
					f.advance(2 * time.Second)
				}
			})
		}
		wg.Wait()

		if f.reads.Load() < 2 {
			t.Errorf("the keys were not read again over %d seconds", 8*100*2)
		}
	})
}

func TestNewNeedsEverything(t *testing.T) {
	complete := Config{Issuer: testIssuer, Audience: testAudience, Subject: testSubject, KeysFile: keysPath}
	if _, err := New(complete); err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]Config{
		"no issuer":   {Audience: testAudience, Subject: testSubject, KeysFile: keysPath},
		"no audience": {Issuer: testIssuer, Subject: testSubject, KeysFile: keysPath},
		"no subject":  {Issuer: testIssuer, Audience: testAudience, KeysFile: keysPath},
		"no key file": {Issuer: testIssuer, Audience: testAudience, Subject: testSubject},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(config); err == nil {
				t.Error("New accepted an incomplete configuration")
			}
		})
	}
}

func TestReadLimited(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) string {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if data, err := readLimited(write("small", 10)); err != nil || len(data) != 10 {
		t.Errorf("a small file: %d bytes, %v", len(data), err)
	}
	if data, err := readLimited(write("huge", maxKeysFileBytes*2)); err != nil || len(data) != maxKeysFileBytes+1 {
		t.Errorf("a huge file came back as %d bytes, %v, want %d", len(data), err, maxKeysFileBytes+1)
	}
	if _, err := readLimited(dir + "/missing"); err == nil {
		t.Error("a missing file was read")
	}
}
