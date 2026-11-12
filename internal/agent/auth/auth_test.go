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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	testIssuer   = "https://kubernetes.default.svc.cluster.local"
	testAudience = "treebind.example.org/apps/dashboards"
	testSubject  = "system:serviceaccount:treebind-system:treebind-controller-manager"

	kidRSA = "rsa-1"
	kidEC  = "ec-1"

	// keysPath is a path the fixture never opens, because it replaces the reader.
	keysPath = "keys.json"

	claimIss = "iss"
	claimSub = "sub"
	claimAud = "aud"
	claimExp = "exp"
	headAlg  = "alg"
	headKid  = "kid"
)

var now = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

// testKeys are generated once, because generating an RSA key is slow.
type testKeys struct {
	rsa, otherRSA *rsa.PrivateKey
	ec            *ecdsa.PrivateKey
}

var keys = sync.OnceValue(func() testKeys {
	mustRSA := func() *rsa.PrivateKey {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		return key
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return testKeys{rsa: mustRSA(), otherRSA: mustRSA(), ec: ec}
})

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func jsonOf(t testing.TB, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// rsaJWK and ecJWK describe public keys the way a cluster publishes them.
func rsaJWK(kid string, key *rsa.PublicKey) jwk {
	return jwk{
		Kty: "RSA", Kid: kid, Use: "sig", Alg: algRS256,
		N: b64(key.N.Bytes()), E: b64(big.NewInt(int64(key.E)).Bytes()),
	}
}

func ecJWK(key *ecdsa.PublicKey) jwk {
	point, err := key.Bytes() // 0x04, then X and Y of 32 bytes each
	if err != nil {
		panic(err)
	}
	return jwk{Kty: "EC", Kid: kidEC, Use: "sig", Alg: algES256, Crv: "P-256", X: b64(point[1:33]), Y: b64(point[33:])}
}

func keyFile(t testing.TB, entries ...jwk) []byte {
	t.Helper()
	return []byte(jsonOf(t, map[string]any{"keys": entries}))
}

// defaultKeyFile holds the RSA and the EC test key.
func defaultKeyFile(t testing.TB) []byte {
	return keyFile(t, rsaJWK(kidRSA, &keys().rsa.PublicKey), ecJWK(&keys().ec.PublicKey))
}

func goodClaims() map[string]any {
	return map[string]any{
		claimIss: testIssuer, claimSub: testSubject, claimAud: []string{testAudience},
		"iat": now.Unix(), "nbf": now.Unix(), claimExp: now.Add(10 * time.Minute).Unix(),
	}
}

// signRS256 and signES256 make a token the way the API server does.
func signRS256(t testing.TB, key *rsa.PrivateKey, header, payload map[string]any) string {
	t.Helper()
	signed := b64([]byte(jsonOf(t, header))) + "." + b64([]byte(jsonOf(t, payload)))
	sum := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64(signature)
}

func signES256(t testing.TB, key *ecdsa.PrivateKey, header, payload map[string]any) string {
	t.Helper()
	signed := b64([]byte(jsonOf(t, header))) + "." + b64([]byte(jsonOf(t, payload)))
	sum := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signed + "." + b64(raw)
}

func rsaToken(t testing.TB, payload map[string]any) string {
	return signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: kidRSA}, payload)
}

// newVerifier returns a verifier whose clock and key file the test controls.
type fixture struct {
	*Verifier
	clock *atomic.Pointer[time.Time]
	file  *atomic.Pointer[[]byte] // nil content makes the file unreadable
	reads atomic.Int64
}

func newFixture(t testing.TB, content []byte) *fixture {
	t.Helper()
	v, err := New(Config{Issuer: testIssuer, Audience: testAudience, Subject: testSubject, KeysFile: keysPath})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{Verifier: v, clock: new(atomic.Pointer[time.Time]), file: new(atomic.Pointer[[]byte])}
	f.clock.Store(&now)
	f.file.Store(&content)
	v.now = func() time.Time { return *f.clock.Load() }
	v.readFile = func(string) ([]byte, error) {
		f.reads.Add(1)
		content := f.file.Load()
		if *content == nil {
			return nil, os.ErrNotExist
		}
		return *content, nil
	}
	return f
}

func (f *fixture) advance(d time.Duration) {
	next := f.clock.Load().Add(d)
	f.clock.Store(&next)
}

func (f *fixture) setFile(content []byte) { f.file.Store(&content) }

func wantError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestVerifyAcceptsTheOperatorsToken(t *testing.T) {
	f := newFixture(t, defaultKeyFile(t))

	t.Run("signed with RS256", func(t *testing.T) {
		if err := f.Verify(rsaToken(t, goodClaims())); err != nil {
			t.Errorf("a good RS256 token was refused: %v", err)
		}
	})

	t.Run("signed with ES256", func(t *testing.T) {
		token := signES256(t, keys().ec, map[string]any{headAlg: algES256, headKid: kidEC}, goodClaims())
		if err := f.Verify(token); err != nil {
			t.Errorf("a good ES256 token was refused: %v", err)
		}
	})

	t.Run("with the testAudience among several", func(t *testing.T) {
		claims := goodClaims()
		claims[claimAud] = []string{"https://other.example", testAudience}
		if err := f.Verify(rsaToken(t, claims)); err != nil {
			t.Errorf("a token that names the testAudience among others was refused: %v", err)
		}
	})

	t.Run("with the testAudience as a string", func(t *testing.T) {
		claims := goodClaims()
		claims[claimAud] = testAudience
		if err := f.Verify(rsaToken(t, claims)); err != nil {
			t.Errorf("a token whose testAudience is a string was refused: %v", err)
		}
	})

	t.Run("as the API server writes the times, with a fraction", func(t *testing.T) {
		claims := goodClaims()
		claims[claimExp] = float64(now.Add(time.Minute).Unix()) + 0.5
		if err := f.Verify(rsaToken(t, claims)); err != nil {
			t.Errorf("a token with a fractional expiry was refused: %v", err)
		}
	})
}

func TestVerifyChecksTheClaims(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]any)
		want   error
	}{
		{"another issuer", func(c map[string]any) { c[claimIss] = "https://evil.example" }, errIssuer},
		{"no issuer", func(c map[string]any) { delete(c, claimIss) }, errIssuer},
		{"another testSubject, as any pod can get", func(c map[string]any) {
			c[claimSub] = "system:serviceaccount:apps:some-pod"
		}, errSubject},
		{"another Treebind's audience", func(c map[string]any) {
			c[claimAud] = []string{"treebind.example.org/apps/other"}
		}, errAudience},
		{"the API server's audience", func(c map[string]any) {
			c[claimAud] = []string{"https://kubernetes.default.svc.cluster.local"}
		}, errAudience},
		{"no audience", func(c map[string]any) { delete(c, claimAud) }, errAudience},
		{"an empty testAudience list", func(c map[string]any) { c[claimAud] = []string{} }, errAudience},
		{"a prefix of the audience", func(c map[string]any) { c[claimAud] = []string{testAudience[:len(testAudience)-1]} }, errAudience},
		{"no expiry, as a token that never ends", func(c map[string]any) { delete(c, claimExp) }, errNoExpiry},
		{"expired beyond the skew", func(c map[string]any) {
			c[claimExp] = now.Add(-DefaultClockSkew - time.Second).Unix()
		}, errExpired},
		{"not valid yet beyond the skew", func(c map[string]any) {
			c["nbf"] = now.Add(DefaultClockSkew + time.Second).Unix()
		}, errNotYetValid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := goodClaims()
			test.change(claims)

			wantError(t, newFixture(t, defaultKeyFile(t)).Verify(rsaToken(t, claims)), test.want)
		})
	}

	t.Run("expired within the skew is accepted, and so is not valid yet", func(t *testing.T) {
		claims := goodClaims()
		claims[claimExp] = now.Add(-DefaultClockSkew + time.Second).Unix()
		claims["nbf"] = now.Add(DefaultClockSkew - time.Second).Unix()

		if err := newFixture(t, defaultKeyFile(t)).Verify(rsaToken(t, claims)); err != nil {
			t.Errorf("refused: %v", err)
		}
	})

	t.Run("an absurd expiry does not overflow into the past", func(t *testing.T) {
		claims := goodClaims()
		claims[claimExp] = 1e300

		if err := newFixture(t, defaultKeyFile(t)).Verify(rsaToken(t, claims)); err != nil {
			t.Errorf("refused: %v", err)
		}
	})

	t.Run("the token ages with the clock", func(t *testing.T) {
		f := newFixture(t, defaultKeyFile(t))
		token := rsaToken(t, goodClaims())
		if err := f.Verify(token); err != nil {
			t.Fatal(err)
		}

		f.advance(10*time.Minute + DefaultClockSkew + time.Second)

		wantError(t, f.Verify(token), errExpired)
	})
}

func TestVerifyRefusesTokensThatAreNotWhatTheyClaim(t *testing.T) {
	f := newFixture(t, defaultKeyFile(t))
	good := rsaToken(t, goodClaims())
	parts := strings.Split(good, ".")

	t.Run("a forged signature, from a key the cluster does not have", func(t *testing.T) {
		forged := signRS256(t, keys().otherRSA, map[string]any{headAlg: algRS256, headKid: kidRSA}, goodClaims())
		wantError(t, f.Verify(forged), errSignature)
	})

	t.Run("a payload changed after signing", func(t *testing.T) {
		claims := goodClaims()
		claims[claimSub] = "system:serviceaccount:apps:some-pod"
		tampered := parts[0] + "." + b64([]byte(jsonOf(t, claims))) + "." + parts[2]
		wantError(t, f.Verify(tampered), errSignature)
	})

	t.Run("a signature with a flipped bit", func(t *testing.T) {
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		signature[0] ^= 1
		wantError(t, f.Verify(parts[0]+"."+parts[1]+"."+b64(signature)), errSignature)
	})

	t.Run("alg none, with no signature", func(t *testing.T) {
		header := b64([]byte(`{"alg":"none","kid":"` + kidRSA + `"}`))
		wantError(t, f.Verify(header+"."+parts[1]+"."), errAlgorithm)
	})

	t.Run("HS256 with the public key as the secret", func(t *testing.T) {
		// The attack that an implementation open to algorithm confusion falls to:
		// the signature is a MAC that anyone can make with the public key.
		header := b64([]byte(`{"alg":"HS256","kid":"` + kidRSA + `"}`))
		signed := header + "." + parts[1]
		mac := hmac.New(sha256.New, keys().rsa.N.Bytes())
		mac.Write([]byte(signed))
		wantError(t, f.Verify(signed+"."+b64(mac.Sum(nil))), errAlgorithm)
	})

	t.Run("an algorithm that is not allowed", func(t *testing.T) {
		header := b64([]byte(`{"alg":"RS384","kid":"` + kidRSA + `"}`))
		wantError(t, f.Verify(header+"."+parts[1]+"."+parts[2]), errAlgorithm)
	})

	t.Run("a header that asks for an extension", func(t *testing.T) {
		header := b64([]byte(`{"alg":"RS256","kid":"` + kidRSA + `","crit":["exp"]}`))
		wantError(t, f.Verify(header+"."+parts[1]+"."+parts[2]), errAlgorithm)
	})

	t.Run("RS256 named for an EC key, and ES256 for an RSA key", func(t *testing.T) {
		rs := signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: kidEC}, goodClaims())
		wantError(t, f.Verify(rs), errKeyMismatch)
		es := signES256(t, keys().ec, map[string]any{headAlg: algES256, headKid: kidRSA}, goodClaims())
		wantError(t, f.Verify(es), errKeyMismatch)
	})

	t.Run("a valid ES256 signature under a header that says RS256", func(t *testing.T) {
		// The signature is good, so only the algorithm check can refuse it.
		header := b64([]byte(jsonOf(t, map[string]any{headAlg: algRS256, headKid: kidEC})))
		signed := header + "." + b64([]byte(jsonOf(t, goodClaims())))
		sum := sha256.Sum256([]byte(signed))
		r, s, err := ecdsa.Sign(rand.Reader, keys().ec, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)

		wantError(t, f.Verify(signed+"."+b64(raw)), errKeyMismatch)
	})

	t.Run("an ES256 signature of the wrong length is a bad signature, not a bad key", func(t *testing.T) {
		token := signES256(t, keys().ec, map[string]any{headAlg: algES256, headKid: kidEC}, goodClaims())
		parts := strings.Split(token, ".")

		wantError(t, f.Verify(parts[0]+"."+parts[1]+"."+b64([]byte("short"))), errSignature)
	})

	t.Run("no key ID", func(t *testing.T) {
		token := signRS256(t, keys().rsa, map[string]any{headAlg: algRS256}, goodClaims())
		wantError(t, f.Verify(token), errNoKeyID)
	})

	t.Run("an unknown key ID", func(t *testing.T) {
		token := signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: "other"}, goodClaims())
		wantError(t, f.Verify(token), errUnknownKey)
	})
}

func TestVerifyRefusesMalformedTokens(t *testing.T) {
	f := newFixture(t, defaultKeyFile(t))
	good := rsaToken(t, goodClaims())
	parts := strings.Split(good, ".")

	tests := map[string]string{
		"nothing":                           "",
		"one segment":                       parts[0],
		"two segments":                      parts[0] + "." + parts[1],
		"four segments":                     good + ".extra",
		"a header that is not JSON":         b64([]byte("not json")) + "." + parts[1] + "." + parts[2],
		"a header that is a list":           b64([]byte(`["RS256"]`)) + "." + parts[1] + "." + parts[2],
		"a segment that is not base64url":   "!!!." + parts[1] + "." + parts[2],
		"a signature that is not base64url": parts[0] + "." + parts[1] + ".!!!",
		"padding in a segment":              parts[0] + "=." + parts[1] + "." + parts[2],
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if err := f.Verify(token); err == nil {
				t.Error("the token was accepted")
			}
		})
	}

	t.Run("a token that is too large is refused before it is parsed", func(t *testing.T) {
		wantError(t, f.Verify(strings.Repeat("a", maxTokenBytes+1)), errTooLarge)
	})

	t.Run("a payload that is not JSON, signed with the right key, is refused", func(t *testing.T) {
		header := b64([]byte(`{"alg":"RS256","kid":"` + kidRSA + `"}`))
		signed := header + "." + b64([]byte("not json"))
		sum := sha256.Sum256([]byte(signed))
		signature, err := rsa.SignPKCS1v15(rand.Reader, keys().rsa, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		wantError(t, f.Verify(signed+"."+b64(signature)), errMalformed)
	})
}

func TestErrorsNameNothingFromTheToken(t *testing.T) {
	f := newFixture(t, defaultKeyFile(t))
	const marker = "attacker-chosen-text-0123456789"

	tokens := map[string]string{
		"an unknown key ID": signRS256(t, keys().rsa, map[string]any{headAlg: algRS256, headKid: marker}, goodClaims()),
		"an issuer":         rsaToken(t, map[string]any{claimIss: marker, claimSub: testSubject, claimAud: testAudience, claimExp: now.Unix() + 60}),
		"a subject":         rsaToken(t, map[string]any{claimIss: testIssuer, claimSub: marker, claimAud: testAudience, claimExp: now.Unix() + 60}),
		"an audience":       rsaToken(t, map[string]any{claimIss: testIssuer, claimSub: testSubject, claimAud: marker, claimExp: now.Unix() + 60}),
		"an algorithm":      b64([]byte(`{"alg":"`+marker+`"}`)) + ".e30.AA",
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			err := f.Verify(token)
			if err == nil {
				t.Fatal("the token was accepted")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the error carries text from the token: %v", err)
			}
		})
	}
}

// The verifier is what the status handler asks.
var _ status.Verifier = (*Verifier)(nil)

func TestServedByTheStatusHandler(t *testing.T) {
	f := newFixture(t, defaultKeyFile(t))
	handler := status.NewHandler(func() status.Report { return status.Report{} }, f)
	var reasons []error
	handler.Rejected = func(reason error) { reasons = append(reasons, reason) }

	get := func(token string) int {
		r := httptest.NewRequest(http.MethodGet, status.Path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	if code := get(rsaToken(t, goodClaims())); code != http.StatusOK {
		t.Errorf("the operator's token got %d, want 200", code)
	}
	forged := signRS256(t, keys().otherRSA, map[string]any{headAlg: algRS256, headKid: kidRSA}, goodClaims())
	if code := get(forged); code != http.StatusUnauthorized {
		t.Errorf("a forged token got %d, want 401", code)
	}
	elsewhere := goodClaims()
	elsewhere[claimAud] = []string{"treebind.example.org/apps/other"}
	if code := get(rsaToken(t, elsewhere)); code != http.StatusUnauthorized {
		t.Errorf("another Treebind's token got %d, want 401", code)
	}

	if len(reasons) != 2 || !errors.Is(reasons[0], errSignature) || !errors.Is(reasons[1], errAudience) {
		t.Errorf("the agent was told %v, want the signature and then the audience", reasons)
	}
}
