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

// Package auth verifies the Kubernetes service-account token with which the
// operator calls the agent's status endpoint (docs/adr/0011). It uses the
// standard library only, and it checks the token without a call to the API
// server, because the agent has no credential to make one with.
//
// A token is accepted when its signature verifies against one of the cluster's
// public keys and its issuer, audience and subject are the expected ones and it
// has not expired. The keys come from a file that holds the JSON Web Key Set the
// cluster publishes. They are public, so the file holds nothing secret.
//
// The errors of Verify are for the agent's own log and are worded without any
// text from the token, which a stranger writes.
package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// maxTokenBytes bounds the token that is parsed. A service-account token is
	// about 1 KiB.
	maxTokenBytes = 8 << 10

	// DefaultMinReload is how often at most the key file is read because a token
	// names a key that is not known. A stranger chooses the key a token names, so
	// without a limit a flood of tokens would be a flood of file reads.
	DefaultMinReload = 10 * time.Second

	// DefaultMaxKeyAge is how long the keys are used before the file is read again,
	// so that a key taken out of the file stops being trusted.
	DefaultMaxKeyAge = 5 * time.Minute

	// DefaultMaxKeyStale is how long keys that cannot be refreshed are still used.
	// Beyond it a file that stays unreadable or damaged stops being a reason to
	// trust a key that may have been taken out of it, and every token is refused
	// until the file is read again.
	DefaultMaxKeyStale = time.Hour

	// emptyRetry is how soon the key file is read again while no key is loaded, so
	// that a file that appears just after a failed read is used within a second.
	emptyRetry = time.Second

	// maxRead is how long a read of the key file may take before it counts as
	// failed. A read that hangs on a bad mount cannot be cancelled, so it is left
	// to finish by itself, and only one such read exists at a time.
	maxRead = 5 * time.Second

	// maxWait is how long a caller waits for a read of the key file that another
	// caller has started, so that a file on a mount that hangs cannot hold every
	// request.
	maxWait = 500 * time.Millisecond

	// DefaultClockSkew is how far the agent's clock may differ from the cluster's
	// before a token counts as expired or not yet valid.
	DefaultClockSkew = 30 * time.Second
)

// The reasons a token is not accepted, worded without anything from the token.
var (
	errTooLarge     = errors.New("the token is too large")
	errMalformed    = errors.New("the token is not three base64url segments of JSON")
	errAlgorithm    = errors.New("the token is not signed with RS256 or ES256, or asks for an extension")
	errNoKeyID      = errors.New("the token names no key")
	errUnknownKey   = errors.New("the token names a key that is not known")
	errKeyMismatch  = errors.New("the key of the token cannot verify its algorithm")
	errSignature    = errors.New("the signature does not verify")
	errIssuer       = errors.New("the issuer is not the cluster's")
	errAudience     = errors.New("the audience is not this agent's")
	errSubject      = errors.New("the subject is not the operator's")
	errNoExpiry     = errors.New("the token has no expiry")
	errExpired      = errors.New("the token has expired")
	errNotYetValid  = errors.New("the token is not valid yet")
	errNoKeysLoaded = errors.New("no keys are loaded")
)

// Config says whose tokens are accepted.
type Config struct {
	// Issuer is the iss claim of a token: the cluster's service-account issuer.
	Issuer string
	// Audience is the aud claim a token has to carry. It names the Treebind, so
	// that a token for one agent is refused by every other.
	Audience string
	// Subject is the sub claim a token has to carry: the operator's service
	// account, as system:serviceaccount:<namespace>:<name>. Any pod can get a token
	// for its own service account with any audience, so the audience alone does not
	// say who is calling.
	Subject string
	// KeysFile is the path of the cluster's JSON Web Key Set.
	KeysFile string

	// MinReload, MaxKeyAge, MaxKeyStale and ClockSkew default to the constants above
	// when zero.
	MinReload   time.Duration
	MaxKeyAge   time.Duration
	MaxKeyStale time.Duration
	ClockSkew   time.Duration
}

// Verifier checks tokens. It is safe for concurrent use.
type Verifier struct {
	issuer, audience, subject, keysFile string
	minReload, maxKeyAge, maxKeyStale   time.Duration
	skew, wait, readLimit               time.Duration

	now      func() time.Time
	readFile func(string) ([]byte, error)

	current atomic.Pointer[keyState]
	// reloading holds one token while the key file is read, so that one caller
	// reads it and the others use the result.
	reloading chan struct{}
	// lastAttempt is when the file was last read, in Unix nanoseconds, and reading
	// is true while a read that was given up on has not returned.
	lastAttempt atomic.Int64
	reading     atomic.Bool
}

// keyState is the keys that are trusted and why the last read of the file, if it
// failed, did not replace them.
type keyState struct {
	keys    *keySet
	lastErr error
}

// noKeys is the state before a first read.
var noKeys = &keyState{}

// New returns a Verifier. It does not read the key file: the file may be put in
// place after the agent starts, and until it is there every token is refused.
func New(config Config) (*Verifier, error) {
	if config.Issuer == "" || config.Audience == "" || config.Subject == "" || config.KeysFile == "" {
		return nil, errors.New("auth: the issuer, the audience, the subject and the key file are all required")
	}
	v := &Verifier{
		issuer:      config.Issuer,
		audience:    config.Audience,
		subject:     config.Subject,
		keysFile:    config.KeysFile,
		minReload:   orDefault(config.MinReload, DefaultMinReload),
		maxKeyAge:   orDefault(config.MaxKeyAge, DefaultMaxKeyAge),
		maxKeyStale: orDefault(config.MaxKeyStale, DefaultMaxKeyStale),
		skew:        orDefault(config.ClockSkew, DefaultClockSkew),
		wait:        maxWait,
		readLimit:   maxRead,
		now:         time.Now,
		readFile:    readLimited,
		reloading:   make(chan struct{}, 1),
	}
	return v, nil
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// readLimited reads a file of at most maxKeysFileBytes. A longer file comes back
// one byte over the limit, which the parser refuses.
func readLimited(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, maxKeysFileBytes+1))
}

// header is the part of a token's header that is looked at.
type header struct {
	Alg  string          `json:"alg"`
	Kid  string          `json:"kid"`
	Crit json.RawMessage `json:"crit"`
}

// claims is the part of a token's payload that is looked at.
type claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    *float64 `json:"exp"`
	NotBefore *float64 `json:"nbf"`
}

// audience is the aud claim, which is a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Verify returns nil when the token is accepted, and otherwise the reason it is
// not.
func (v *Verifier) Verify(token string) error {
	if len(token) > maxTokenBytes {
		return errTooLarge
	}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return errMalformed
	}

	var head header
	if err := decodeJSON(segments[0], &head); err != nil {
		return errMalformed
	}
	if (head.Alg != algRS256 && head.Alg != algES256) || len(head.Crit) > 0 {
		return errAlgorithm
	}
	if head.Kid == "" {
		return errNoKeyID
	}
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		return errMalformed
	}

	key, err := v.key(head.Kid)
	if err != nil {
		return err
	}
	if err := verifySignature(key, head.Alg, segments[0]+"."+segments[1], signature); err != nil {
		return err
	}

	// The claims are read only once the signature has verified.
	var payload claims
	if err := decodeJSON(segments[1], &payload); err != nil {
		return errMalformed
	}
	return v.checkClaims(payload)
}

// decodeJSON decodes a base64url segment that holds a JSON object.
func decodeJSON(segment string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// verifySignature checks a signature with a key of the type the algorithm needs.
// A key of another type is refused, which is what stops a token from naming an
// algorithm that makes a public key act as a secret.
func verifySignature(key crypto.PublicKey, alg, signed string, signature []byte) error {
	sum := sha256.Sum256([]byte(signed))
	switch public := key.(type) {
	case *rsa.PublicKey:
		if alg != algRS256 {
			return errKeyMismatch
		}
		if rsa.VerifyPKCS1v15(public, crypto.SHA256, sum[:], signature) != nil {
			return errSignature
		}
	case *ecdsa.PublicKey:
		if alg != algES256 {
			return errKeyMismatch
		}
		if len(signature) != 64 {
			return errSignature
		}
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		if !ecdsa.Verify(public, sum[:], r, s) {
			return errSignature
		}
	default:
		return errKeyMismatch
	}
	return nil
}

// checkClaims checks the claims of a token whose signature verified.
func (v *Verifier) checkClaims(c claims) error {
	if c.Issuer != v.issuer {
		return errIssuer
	}
	if c.Subject != v.subject {
		return errSubject
	}
	named := false
	for _, aud := range c.Audience {
		named = named || aud == v.audience
	}
	if !named {
		return errAudience
	}

	now := v.now()
	if c.Expiry == nil {
		return errNoExpiry
	}
	if now.After(numericDate(*c.Expiry).Add(v.skew)) {
		return errExpired
	}
	if c.NotBefore != nil && numericDate(*c.NotBefore).After(now.Add(v.skew)) {
		return errNotYetValid
	}
	return nil
}

// numericDate converts a JWT NumericDate, in seconds and possibly fractional. A
// value outside a sensible range is held to its edge, so that a huge number
// cannot overflow the conversion.
func numericDate(seconds float64) time.Time {
	const limit = 4e12
	seconds = min(max(seconds, -limit), limit)
	whole := int64(seconds)
	return time.Unix(whole, int64((seconds-float64(whole))*1e9))
}

// key returns the public key a token names. Keys that are older than the maximum
// age, and a key that is not among those loaded, make the key file be read, at
// most once in the minimum interval whoever asks.
func (v *Verifier) key(kid string) (crypto.PublicKey, error) {
	state := v.stateOrEmpty()
	switch {
	case state.keys == nil:
		// Nothing is loaded, so the caller waits for the read that is under way.
		state = v.reload(true)
	case v.now().Sub(state.keys.loadedAt) >= v.maxKeyAge:
		// The keys can be used while another caller reads the file.
		state = v.reload(false)
	}

	key, known := v.lookup(state, kid)
	if state.keys != nil && !known {
		// A key that is not known may be a new one, and the read that is under way
		// may bring it, so the caller waits for it.
		state = v.reload(true)
		key, known = v.lookup(state, kid)
	}

	switch {
	case known:
		return key, nil
	case v.usable(state) == nil:
		if state.lastErr != nil {
			return nil, fmt.Errorf("%w: %w", errNoKeysLoaded, state.lastErr)
		}
		return nil, errNoKeysLoaded
	default:
		return nil, errUnknownKey
	}
}

// usable returns the keys of a state, or nil when there are none or they have gone
// too long without a successful read, whether or not a read was tried just now.
func (v *Verifier) usable(state *keyState) *keySet {
	if state.keys == nil || v.now().Sub(state.keys.loadedAt) >= v.maxKeyStale {
		return nil
	}
	return state.keys
}

// lookup returns the key of an ID among the keys that may be used.
func (v *Verifier) lookup(state *keyState, kid string) (crypto.PublicKey, bool) {
	keys := v.usable(state)
	if keys == nil {
		return nil, false
	}
	key, ok := keys.byKID[kid]
	return key, ok
}

// acquire takes the right to read the key file. A caller that is not to wait, or
// that has waited as long as it may, gives up and uses the keys that are loaded.
func (v *Verifier) acquire(wait bool) bool {
	select {
	case v.reloading <- struct{}{}:
		return true
	default:
	}
	if !wait {
		return false
	}
	timer := time.NewTimer(v.wait)
	defer timer.Stop()
	select {
	case v.reloading <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// recentAttempt reports whether the file was read so recently that it is not read
// again yet. While no key is loaded the interval is shorter.
func (v *Verifier) recentAttempt(now time.Time, state *keyState) bool {
	last := v.lastAttempt.Load()
	if last == 0 {
		return false
	}
	interval := v.minReload
	if state.keys == nil {
		interval = min(interval, emptyRetry)
	}
	return now.Sub(time.Unix(0, last)) < interval
}

// reload reads the key file unless that was done recently, and returns the keys
// that are trusted afterwards. A caller that finds a read under way waits for it
// when it may, because the keys it needs may be what that read brings. A file that
// cannot be read or parsed leaves the keys that were loaded in place, for as long
// as the maximum staleness allows, so that a fault in the file does not shut the
// operator out.
func (v *Verifier) reload(wait bool) *keyState {
	if !v.acquire(wait) {
		return v.stateOrEmpty()
	}
	defer func() { <-v.reloading }()

	// The time is taken now, not before the wait, and so are the keys.
	now := v.now()
	previous := v.stateOrEmpty()
	if v.recentAttempt(now, previous) {
		return previous
	}
	v.lastAttempt.Store(now.UnixNano())

	data, err := v.read()
	if err != nil {
		return v.keep(previous, err)
	}
	keys, err := parseKeys(data, now)
	if err != nil {
		return v.keep(previous, err)
	}
	next := &keyState{keys: keys}
	v.current.Store(next)
	return next
}

// read reads the key file, giving up on it when it takes too long. The read goes
// on in the background until it returns, and its result is dropped. While it is
// outstanding no other read is started, so a mount that hangs costs one goroutine.
func (v *Verifier) read() ([]byte, error) {
	if !v.reading.CompareAndSwap(false, true) {
		return nil, errors.New("an earlier read of the key file has not returned")
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := v.readFile(v.keysFile)
		v.reading.Store(false)
		done <- result{data, err}
	}()

	timer := time.NewTimer(v.readLimit)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, errors.New("the key file cannot be read")
		}
		return r.data, nil
	case <-timer.C:
		return nil, errors.New("the key file did not answer in time")
	}
}

// keep records why the file did not replace the keys, and returns the state.
func (v *Verifier) keep(previous *keyState, reason error) *keyState {
	next := &keyState{keys: previous.keys, lastErr: reason}
	v.current.Store(next)
	return next
}

func (v *Verifier) stateOrEmpty() *keyState {
	if state := v.current.Load(); state != nil {
		return state
	}
	return noKeys
}
