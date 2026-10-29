package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// LocalIssuerName is the iss claim of tokens this API mints itself. It is
// the same identity the users table records for a local user, so the chain
// verifier can route a token to the right verifier by issuer alone.
const LocalIssuerName = "beelinewf-local"

// localLeeway tolerates serialization rounding and same-fleet clock drift
// on a local token's lifetime. It never relaxes the signature, issuer, or
// audience checks.
const localLeeway = time.Minute

// LocalAuthenticator mints and verifies the API's own HS256 JWTs. It holds
// no session state: like the OIDC verifier it only signs at login and
// checks a signature on the way in, so every replica verifies with the
// same shared secret.
type LocalAuthenticator struct {
	secret   []byte
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time
}

// NewLocalAuthenticator builds the issuer/verifier pair. The secret is the
// dedicated HMAC key (at least 32 bytes, never the API token or the
// at-rest encryption key); ttl bounds every issued token.
func NewLocalAuthenticator(secret, issuer, audience string, ttl time.Duration) (*LocalAuthenticator, error) {
	if len(secret) < 32 {
		return nil, errors.New("auth: local jwt secret must be at least 32 bytes")
	}
	if strings.TrimSpace(issuer) == "" {
		return nil, errors.New("auth: local issuer is required")
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("auth: local audience is required")
	}
	if ttl <= 0 {
		return nil, errors.New("auth: local token ttl must be > 0")
	}
	return &LocalAuthenticator{
		secret:   []byte(secret),
		issuer:   strings.TrimSpace(issuer),
		audience: strings.TrimSpace(audience),
		ttl:      ttl,
		now:      time.Now,
	}, nil
}

// localClaims is the signed shape of a local JWT. Subject is the users.id
// uuid; roles are the stored roles read at login, mirroring how an OIDC
// token carries its roles on the claims.
type localClaims struct {
	jwt.Claims
	Name  string   `json:"name,omitempty"`
	Email string   `json:"email,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

// Issue signs a token for a local user. Roles are embedded at login time,
// so a role change takes effect at the next login — the same window an
// OIDC caller has between provider refreshes.
func (a *LocalAuthenticator) Issue(userID, name, email string, roles []string) (token string, expiresAt time.Time, err error) {
	if a == nil {
		return "", time.Time{}, errors.New("auth: local authenticator is nil")
	}
	if strings.TrimSpace(userID) == "" {
		return "", time.Time{}, errors.New("auth: local issue requires a user id")
	}
	now := a.now().UTC()
	expiresAt = now.Add(a.ttl)
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: a.secret},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: local signer: %w", err)
	}
	raw, err := jwt.Signed(signer).Claims(&localClaims{
		Claims: jwt.Claims{
			Issuer:   a.issuer,
			Subject:  userID,
			Audience: jwt.Audience{a.audience},
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(expiresAt),
		},
		Name:  name,
		Email: email,
		Roles: roles,
	}).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: local issue: %w", err)
	}
	return raw, expiresAt, nil
}

// Verify validates the signature, issuer, audience, and expiry of rawToken
// and projects the claims onto a Principal. Every failure is reported as
// ErrUnauthenticated, exactly like the OIDC verifier.
func (a *LocalAuthenticator) Verify(_ context.Context, rawToken string) (Principal, error) {
	if a == nil {
		return Principal{}, fmt.Errorf("%w: local auth is disabled", ErrUnauthenticated)
	}
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return Principal{}, fmt.Errorf("%w: empty Bearer [REDACTED]", ErrUnauthenticated)
	}
	parsed, err := jwt.ParseSigned(rawToken, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	var claims localClaims
	if err := parsed.Claims(a.secret, &claims); err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	if err := claims.ValidateWithLeeway(jwt.Expected{
		Issuer:      a.issuer,
		AnyAudience: jwt.Audience{a.audience},
		Time:        a.now(),
	}, localLeeway); err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return Principal{}, fmt.Errorf("%w: subject is empty", ErrUnauthenticated)
	}
	// The principal carries the row marker, not the token issuer: like an
	// OIDC principal (whose issuer is the provider URL recorded on its
	// row), a local principal's issuer is the "local" recorded on its
	// row, even when the operator overrides the token iss claim. The
	// token issuer was already verified above; this is the identity the
	// resolver and the audit trail agree on.
	return Principal{
		Subject: claims.Subject,
		Issuer:  model.LocalIssuer,
		Name:    firstNonEmpty(strings.TrimSpace(claims.Name), strings.TrimSpace(claims.Email), claims.Subject),
		Email:   strings.TrimSpace(claims.Email),
		Roles:   dedupeRoles(claims.Roles),
	}, nil
}

// IssuerName reports the iss claim this verifier accepts, so the chain
// verifier can route a token to it before doing any crypto.
func (a *LocalAuthenticator) IssuerName() string {
	if a == nil {
		return ""
	}
	return a.issuer
}

// Config reports OIDC as disabled: the AuthConfig shape is the OIDC login
// contract (provider endpoints a frontend runs code+PKCE against), and
// local auth has no provider. The handler serves the local fragment from
// configuration directly. Without this, a local-only deployment would
// advertise OIDC enabled with the local issuer, sending a frontend to run
// a provider flow against an issuer that serves no discovery.
func (a *LocalAuthenticator) Config() AuthConfig {
	return AuthConfig{}
}

// dedupeRoles drops blank and repeated roles while keeping the token's
// order, matching the OIDC claim reader.
func dedupeRoles(roles []string) []string {
	seen := make(map[string]bool, len(roles))
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		out = append(out, role)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NewChainVerifier composes the OIDC and local verifiers behind one
// Verifier, routing by the token's unverified iss claim. Either side may
// be nil: with one side off the chain is that side. The peek is routing
// only — the chosen verifier still fully verifies, so a forged issuer
// buys nothing and every combination fails closed.
func NewChainVerifier(oidc Verifier, local *LocalAuthenticator) Verifier {
	if oidc == nil && local == nil {
		return nil
	}
	if oidc == nil {
		return local
	}
	if local == nil {
		return oidc
	}
	return &chainVerifier{oidc: oidc, local: local}
}

type chainVerifier struct {
	oidc  Verifier
	local *LocalAuthenticator
}

func (c *chainVerifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	if c.local != nil && peekIssuer(rawToken) == c.local.IssuerName() {
		return c.local.Verify(ctx, rawToken)
	}
	return c.oidc.Verify(ctx, rawToken)
}

// Config returns the OIDC login contract: the local contract has no
// provider endpoints, so there is nothing of it to advertise here.
func (c *chainVerifier) Config() AuthConfig {
	if c.oidc == nil {
		return AuthConfig{}
	}
	return c.oidc.Config()
}

// peekIssuer reads the iss claim without verifying the signature. It
// answers "" for anything that is not a three-part JWT with a JSON
// object payload, which routes the token to the OIDC verifier — and
// there it is rejected as malformed.
func peekIssuer(rawToken string) string {
	parts := strings.Split(strings.TrimSpace(rawToken), ".")
	if len(parts) != 3 || parts[1] == "" {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Issuer
}

// verificationTokenBytes is the entropy of an email verification token:
// 32 random bytes, base64url-encoded for the link.
const verificationTokenBytes = 32

// GenerateVerificationToken mints a single-use email verification token,
// returning the raw token for the email and its SHA-256 hex for storage.
// The raw token never touches the database.
func GenerateVerificationToken() (raw, hash string, err error) {
	buf := make([]byte, verificationTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: verification token entropy: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashVerificationToken(raw), nil
}

// HashVerificationToken hashes a presented token for the database lookup.
func HashVerificationToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
