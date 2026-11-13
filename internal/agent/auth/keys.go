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
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"time"
)

const (
	// maxKeysFileBytes bounds the key file that is read and parsed.
	maxKeysFileBytes = 1 << 20

	// minRSABits and maxRSABits bound the RSA keys that are trusted. A cluster uses
	// 2048 or 4096 bits. The upper bound keeps a key in the file from making every
	// check expensive.
	minRSABits = 2048
	maxRSABits = 8192

	// maxRSAExponent bounds the public exponent. The usual value is 65537.
	maxRSAExponent = 1<<31 - 1

	algRS256 = "RS256"
	algES256 = "ES256"
)

// keySet is the public keys of a cluster, by key ID, as read at one time.
type keySet struct {
	byKID    map[string]crypto.PublicKey
	loadedAt time.Time
}

// jwks is the JSON Web Key Set that a cluster publishes at /openid/v1/jwks.
type jwks struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// CheckKeys returns nil when the agent could use data as its key file, and the
// reason when it could not. The operator calls it on the key set it is about to
// publish, so that a page that is not a key set never replaces keys that work.
func CheckKeys(data []byte) error {
	_, err := parseKeys(data, time.Time{})
	return err
}

// parseKeys reads a key set. A key of a type, a size or a use this package does
// not accept is skipped, so that a cluster that also publishes other keys works.
// Two different keys under one key ID are an error, because which key a token
// means would be a guess. A file with no usable key is an error.
func parseKeys(data []byte, loadedAt time.Time) (*keySet, error) {
	if len(data) > maxKeysFileBytes {
		return nil, errors.New("the key file is too large")
	}
	var set jwks
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, errors.New("the key file is not a JSON Web Key Set")
	}

	keys := &keySet{byKID: make(map[string]crypto.PublicKey, len(set.Keys)), loadedAt: loadedAt}
	for _, entry := range set.Keys {
		key, ok := entry.publicKey()
		if !ok {
			continue
		}
		if known, duplicate := keys.byKID[entry.Kid]; duplicate {
			// The same key twice is harmless. Two keys under one ID make it a guess
			// which one a token means.
			if equal, ok := known.(interface{ Equal(crypto.PublicKey) bool }); ok && equal.Equal(key) {
				continue
			}
			return nil, errors.New("the key file has two different keys under one key ID")
		}
		keys.byKID[entry.Kid] = key
	}
	if len(keys.byKID) == 0 {
		return nil, errors.New("the key file has no usable key")
	}
	return keys, nil
}

// publicKey returns the key of an entry that this package accepts: a signing key
// with an ID, which is RSA of at least 2048 bits for RS256 or P-256 for ES256. An
// entry that names an algorithm has to name the one its key type uses.
func (k jwk) publicKey() (crypto.PublicKey, bool) {
	if k.Kid == "" || (k.Use != "" && k.Use != "sig") {
		return nil, false
	}
	switch k.Kty {
	case "RSA":
		if k.Alg != "" && k.Alg != algRS256 {
			return nil, false
		}
		return k.rsaKey()
	case "EC":
		if k.Alg != "" && k.Alg != algES256 {
			return nil, false
		}
		return k.ecKey()
	}
	return nil, false
}

func (k jwk) rsaKey() (crypto.PublicKey, bool) {
	n, ok := bigInt(k.N)
	if !ok || n.BitLen() < minRSABits || n.BitLen() > maxRSABits {
		return nil, false
	}
	e, ok := bigInt(k.E)
	if !ok || !e.IsInt64() || e.Int64() < 3 || e.Int64() > maxRSAExponent || e.Bit(0) == 0 {
		return nil, false
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, true
}

func (k jwk) ecKey() (crypto.PublicKey, bool) {
	if k.Crv != "P-256" {
		return nil, false
	}
	x, xerr := base64.RawURLEncoding.DecodeString(k.X)
	y, yerr := base64.RawURLEncoding.DecodeString(k.Y)
	if xerr != nil || yerr != nil || len(x) != 32 || len(y) != 32 {
		return nil, false
	}
	point := append(append([]byte{4}, x...), y...)
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, false
	}
	return key, true
}

// bigInt decodes a base64url integer with no padding, as a JWK writes it.
func bigInt(s string) (*big.Int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return new(big.Int).SetBytes(raw), true
}
