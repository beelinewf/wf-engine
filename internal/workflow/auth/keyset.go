package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// minKeySetTTL is the floor on how often the JWKS is re-read.
// auth.oidc.cache_ttl can raise it, never lower it, because a shorter window
// is the provider-traffic amplification this type exists to prevent.
const minKeySetTTL = time.Minute

// ttlKeySet is the enforcement point for auth.oidc.cache_ttl: it owns the
// fetch of the provider's JWKS and re-reads it at most once per TTL.
//
// go-oidc's own RemoteKeySet re-reads the jwks_uri on *every* failed
// verification and never expires what it has. The first half makes the
// identity provider an amplification target: the key set is unauthenticated
// input, so anyone who can reach the API can send garbage bearer tokens and
// force one outbound fetch each. A rejected request should not become a
// request to the provider. The second half is the ordinary risk of a
// process-lifetime cache: a key the provider has withdrawn keeps verifying
// until the process restarts.
//
// So this type holds the parsed keys, verifies against them locally, and
// re-reads the document once per TTL. A flood of bad tokens costs one fetch
// per TTL rather than one per request, and a rotated key is picked up at the
// next refresh even if no request ever fails.
//
// Note that the signature algorithm, issuer, audience, and expiry checks all
// live in the go-oidc verifier above this type. What is returned here is the
// *unverified* payload, which the verifier then checks; this type's only
// responsibility is the cryptographic signature.
type ttlKeySet struct {
	jwksURL string
	ttl     time.Duration
	client  *http.Client
	// now is the clock the TTL is measured against. Tests substitute a
	// controllable one so the window can be crossed without sleeping.
	now func() time.Time

	mu        sync.RWMutex
	keys      []jose.JSONWebKey
	fetchedAt time.Time
	// rereadAt is when the keys were last read because a token named a key
	// the cache did not hold, as opposed to the TTL expiring. It throttles
	// that path independently: a rotation re-read is worth spending the
	// first time, but not once per bad token.
	rereadAt time.Time
	// callsMu guards call: at most one JWKS fetch is in flight, shared by
	// concurrent refreshers (singleflight). The fetch itself runs outside
	// any lock so a slow provider stalls waiters on a channel, not on a
	// mutex, and a failed fetch is shared rather than retried per waiter.
	callsMu sync.Mutex
	call    *refreshCall
}

// refreshCall is one in-flight JWKS fetch shared by concurrent refreshers.
type refreshCall struct {
	done     chan struct{}
	keys     []jose.JSONWebKey
	ok       bool
	rotation bool
}

// newTTLKeySet wraps the provider's JWKS in a lifetime bound. A ttl at or
// below minKeySetTTL collapses to the floor.
func newTTLKeySet(jwksURL string, ttl time.Duration) *ttlKeySet {
	return newTTLKeySetWithClock(jwksURL, ttl, time.Now)
}

// newTTLKeySetWithClock is newTTLKeySet with an injectable clock.
func newTTLKeySetWithClock(jwksURL string, ttl time.Duration, now func() time.Time) *ttlKeySet {
	if ttl < minKeySetTTL {
		ttl = minKeySetTTL
	}
	return &ttlKeySet{
		jwksURL: jwksURL,
		ttl:     ttl,
		client:  &http.Client{Timeout: 10 * time.Second},
		now:     now,
	}
}

// VerifySignature implements oidc.KeySet.
func (k *ttlKeySet) VerifySignature(ctx context.Context, token string) ([]byte, error) {
	jws, err := jose.ParseSigned(token, allowedSignatureAlgorithms)
	if err != nil {
		// Parsing fails before any key lookup, so a syntactically broken
		// token costs the provider nothing.
		return nil, fmt.Errorf("oidc: malformed jwt: %w", err)
	}
	// A JWS carrying no signature at all has nothing to verify. The parser
	// rejects the usual shapes, but the length is what verify() and
	// UnsafePayloadWithoutVerification() index into, so it is checked here
	// rather than assumed.
	if len(jws.Signatures) == 0 {
		return nil, errors.New("oidc: jwt carries no signature")
	}
	if k.stale() {
		k.refresh(ctx)
	}
	if err := k.verify(jws); err != nil {
		// The cached keys may simply predate a provider rotation. One
		// throttled re-read turns a key the provider published mid-TTL from
		// a window of refusals into a single extra fetch; the throttle keeps
		// the re-read from becoming the amplification this type exists to
		// prevent, so a genuinely bad token still costs at most one provider
		// request per TTL.
		k.refreshForRotation(ctx, jws.Signatures[0].Header.KeyID)
		if rerr := k.verify(jws); rerr != nil {
			return nil, errors.New("failed to verify id token signature")
		}
	}
	return jws.UnsafePayloadWithoutVerification(), nil
}

// allowedSignatureAlgorithms mirrors the asymmetric half of the set go-oidc
// accepts. The HMAC algorithms are deliberately absent: a JWKS publishes
// public keys, and an HS* token is verified with a *shared secret*, so
// accepting one would mean verifying a caller-supplied token against a key
// an attacker chooses. The verifier pins the provider's advertised
// algorithms separately; this is only the parser's accept list, so a token
// has to parse before it can be checked.
var allowedSignatureAlgorithms = []jose.SignatureAlgorithm{
	jose.EdDSA,
	jose.RS256,
	jose.RS384,
	jose.RS512,
	jose.ES256,
	jose.ES384,
	jose.ES512,
	jose.PS256,
	jose.PS384,
	jose.PS512,
}

// verify checks a parsed signature against the cached keys.
func (k *ttlKeySet) verify(jws *jose.JSONWebSignature) error {
	kid := jws.Signatures[0].Header.KeyID
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, key := range k.keys {
		if kid != "" && key.KeyID != kid {
			continue
		}
		if _, err := jws.Verify(&key); err == nil {
			return nil
		}
	}
	return errors.New("no cached key verified the signature")
}

// stale reports whether the held keys have aged past the TTL.
func (k *ttlKeySet) stale() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.fetchedAt.IsZero() || k.now().Sub(k.fetchedAt) >= k.ttl
}

// refresh re-reads the JWKS. Concurrent callers collapse onto one fetch. A
// failure is deliberately not surfaced: an unreachable provider leaves the
// previous keys in place, so callers holding a still-valid token keep working
// instead of the whole deployment losing authentication at once.
func (k *ttlKeySet) refresh(ctx context.Context) {
	if !k.stale() {
		return
	}
	k.doRefresh(ctx, false, k.stale)
}

// doRefresh runs one shared JWKS fetch, joining the in-flight call when
// another refresher already started one. The fetch runs outside any lock.
// recheck re-validates the need after acquiring the call lock, so a
// refresher that queued behind a completed fetch does not start another.
func (k *ttlKeySet) doRefresh(ctx context.Context, rotation bool, recheck func() bool) {
	k.callsMu.Lock()
	if k.call != nil {
		cl := k.call
		if rotation {
			cl.rotation = true
		}
		k.callsMu.Unlock()
		<-cl.done
		return
	}
	if !recheck() {
		k.callsMu.Unlock()
		return
	}
	cl := &refreshCall{done: make(chan struct{}), rotation: rotation}
	k.call = cl
	k.callsMu.Unlock()

	keys, ok := k.fetch(ctx)

	k.mu.Lock()
	if ok {
		k.keys = keys
		k.fetchedAt = k.now()
		if cl.rotation {
			k.rereadAt = k.fetchedAt
		}
	}
	k.mu.Unlock()

	k.callsMu.Lock()
	k.call = nil
	k.callsMu.Unlock()
	cl.keys = keys
	cl.ok = ok
	close(cl.done)
}

// refreshForRotation re-reads the JWKS even though the TTL has not passed,
// so a key the provider published after the last read is picked up. It runs
// only for a token naming a key id no cached key carries, which is what a
// rotation looks like; a token that fails against a key the cache does hold
// is not given a second chance, so a forged or withdrawn token never spends
// a provider request. The re-read is throttled to one per TTL, which is the
// same bound the TTL path gets, so a flood of bad tokens still reaches the
// provider at the rate this type exists to enforce.
func (k *ttlKeySet) refreshForRotation(ctx context.Context, kid string) {
	if kid == "" {
		return
	}
	k.mu.RLock()
	known := false
	for _, key := range k.keys {
		if key.KeyID == kid {
			known = true
			break
		}
	}
	// A re-read already happened inside this window: spend nothing more.
	spent := !k.rereadAt.IsZero() && k.now().Sub(k.rereadAt) < k.ttl
	k.mu.RUnlock()
	if known || spent {
		return
	}
	k.doRefresh(ctx, true, func() bool {
		k.mu.RLock()
		defer k.mu.RUnlock()
		for _, key := range k.keys {
			if key.KeyID == kid {
				return false
			}
		}
		return k.rereadAt.IsZero() || k.now().Sub(k.rereadAt) >= k.ttl
	})
}

// fetch reads and parses the JWKS document.
func (k *ttlKeySet) fetch(ctx context.Context) ([]jose.JSONWebKey, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.jwksURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var doc struct {
		Keys []jose.JSONWebKey `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || len(doc.Keys) == 0 {
		return nil, false
	}
	return doc.Keys, true
}
