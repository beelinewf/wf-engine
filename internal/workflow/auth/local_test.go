package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

const testLocalSecret = "0123456789abcdef0123456789abcdef"

func testLocalAuth(t *testing.T) *LocalAuthenticator {
	t.Helper()
	a, err := NewLocalAuthenticator(testLocalSecret, LocalIssuerName, "beelinewf", time.Hour)
	if err != nil {
		t.Fatalf("NewLocalAuthenticator() error = %v", err)
	}
	return a
}

func TestNewLocalAuthenticatorRejectsWeakConfig(t *testing.T) {
	cases := []struct {
		name     string
		secret   string
		issuer   string
		audience string
		ttl      time.Duration
	}{
		{"short secret", "short", LocalIssuerName, "beelinewf", time.Hour},
		{"blank issuer", testLocalSecret, "  ", "beelinewf", time.Hour},
		{"blank audience", testLocalSecret, LocalIssuerName, "", time.Hour},
		{"zero ttl", testLocalSecret, LocalIssuerName, "beelinewf", 0},
	}
	for _, tc := range cases {
		if _, err := NewLocalAuthenticator(tc.secret, tc.issuer, tc.audience, tc.ttl); err == nil {
			t.Errorf("%s: error = nil, want an error", tc.name)
		}
	}
}

// Issue then verify is the whole local session: the principal carries the
// users.id subject, the local issuer, and the stored roles.
func TestLocalIssueVerifyRoundTrip(t *testing.T) {
	a := testLocalAuth(t)
	token, expires, err := a.Issue("11111111-1111-7111-8111-111111111111", "Ada", "ada@example.test", []string{"admin", "  ", "admin"})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if time.Until(expires) <= 0 || time.Until(expires) > time.Hour {
		t.Errorf("expires = %v, want ~1h in the future", expires)
	}
	p, err := a.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	// The principal carries the row marker, not the token issuer.
	if p.Subject != "11111111-1111-7111-8111-111111111111" || p.Issuer != model.LocalIssuer {
		t.Errorf("subject/issuer = %q/%q", p.Subject, p.Issuer)
	}
	if p.Name != "Ada" || p.Email != "ada@example.test" {
		t.Errorf("name/email = %q/%q", p.Name, p.Email)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin] deduped", p.Roles)
	}
	if p.Service || p.UserID != "" {
		t.Errorf("service/userID = %v/%q, want false/empty (resolution fills UserID)", p.Service, p.UserID)
	}
}

// Every untrustworthy token is ErrUnauthenticated, exactly like the OIDC
// verifier, so the middleware maps them all to 401.
func TestLocalVerifyRejects(t *testing.T) {
	a := testLocalAuth(t)
	token, _, err := a.Issue("11111111-1111-7111-8111-111111111111", "Ada", "ada@example.test", nil)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	wrongSecret, err := NewLocalAuthenticator("fedcba9876543210fedcba9876543210", LocalIssuerName, "beelinewf", time.Hour)
	if err != nil {
		t.Fatalf("NewLocalAuthenticator() error = %v", err)
	}
	wrongAudience, err := NewLocalAuthenticator(testLocalSecret, LocalIssuerName, "other", time.Hour)
	if err != nil {
		t.Fatalf("NewLocalAuthenticator() error = %v", err)
	}

	tampered := token[:len(token)-2] + "xx"
	cases := map[string]func() (Principal, error){
		"empty":        func() (Principal, error) { return a.Verify(context.Background(), "  ") },
		"garbage":      func() (Principal, error) { return a.Verify(context.Background(), "not-a-jwt") },
		"tampered":     func() (Principal, error) { return a.Verify(context.Background(), tampered) },
		"wrong secret": func() (Principal, error) { return wrongSecret.Verify(context.Background(), token) },
		"wrong audience": func() (Principal, error) {
			return wrongAudience.Verify(context.Background(), token)
		},
	}
	for name, verify := range cases {
		_, err := verify()
		if !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: error = %v, want ErrUnauthenticated", name, err)
		}
	}
}

// A token past its lifetime plus leeway is rejected; one inside the leeway
// still passes, pinning the rounding tolerance.
func TestLocalVerifyExpiry(t *testing.T) {
	a := testLocalAuth(t)
	now := time.Now().UTC()
	a.now = func() time.Time { return now }
	token, expires, err := a.Issue("11111111-1111-7111-8111-111111111111", "Ada", "ada@example.test", nil)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	a.now = func() time.Time { return expires.Add(30 * time.Second) }
	if _, err := a.Verify(context.Background(), token); err != nil {
		t.Errorf("Verify() inside leeway error = %v, want nil", err)
	}
	a.now = func() time.Time { return expires.Add(2 * time.Minute) }
	if _, err := a.Verify(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Verify() past leeway error = %v, want ErrUnauthenticated", err)
	}
}

func TestLocalNameFallbacks(t *testing.T) {
	a := testLocalAuth(t)
	token, _, err := a.Issue("11111111-1111-7111-8111-111111111111", "", "ada@example.test", nil)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	p, err := a.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if p.Name != "ada@example.test" {
		t.Errorf("name = %q, want the email fallback", p.Name)
	}
	nameless, _, err := a.Issue("11111111-1111-7111-8111-111111111111", "", "", nil)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	p, err = a.Verify(context.Background(), nameless)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if p.Name != "11111111-1111-7111-8111-111111111111" {
		t.Errorf("name = %q, want the subject fallback", p.Name)
	}
}

// stubVerifier records whether the chain routed a token to it.
type stubVerifier struct {
	principal Principal
	err       error
	calls     int
}

func (s *stubVerifier) Verify(context.Context, string) (Principal, error) {
	s.calls++
	return s.principal, s.err
}

func (s *stubVerifier) Config() AuthConfig {
	return AuthConfig{Enabled: true, Issuer: "https://stub.example.test"}
}

// The chain routes by unverified issuer, then fully verifies: a forged
// issuer reaches the named verifier and is rejected there, never admitted.
func TestChainVerifierRouting(t *testing.T) {
	local := testLocalAuth(t)
	token, _, err := local.Issue("11111111-1111-7111-8111-111111111111", "Ada", "ada@example.test", []string{"admin"})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	oidc := &stubVerifier{err: errors.New("oidc rejects")}
	chain := NewChainVerifier(oidc, local)

	p, err := chain.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("chain local token error = %v", err)
	}
	if p.Issuer != model.LocalIssuer || oidc.calls != 0 {
		t.Errorf("local token routed wrong: issuer=%q oidc.calls=%d", p.Issuer, oidc.calls)
	}

	// Garbage has no issuer, so it falls through to OIDC and is rejected.
	if _, err := chain.Verify(context.Background(), "garbage"); err == nil {
		t.Error("chain garbage error = nil, want rejection")
	}
	if oidc.calls != 1 {
		t.Errorf("oidc.calls = %d, want 1 for the garbage token", oidc.calls)
	}

	// A forged local issuer with a bad signature reaches the local
	// verifier, which rejects it: the peek never admits anything.
	forged := token[:len(token)-2] + "xx"
	if _, err := chain.Verify(context.Background(), forged); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("forged local token error = %v, want ErrUnauthenticated", err)
	}
}

// A local-only chain must not advertise OIDC: the contract shape carries
// provider endpoints, and a frontend would run a code+PKCE flow against
// an issuer that serves no discovery.
func TestChainVerifierLocalOnlyReportsOIDCDisabled(t *testing.T) {
	chain := NewChainVerifier(nil, testLocalAuth(t))
	if chain.Config().Enabled {
		t.Errorf("local-only chain Config() = %+v, want OIDC disabled", chain.Config())
	}
}

func TestChainVerifierNilSides(t *testing.T) {
	if NewChainVerifier(nil, nil) != nil {
		t.Error("NewChainVerifier(nil, nil) != nil")
	}
	local := testLocalAuth(t)
	if NewChainVerifier(nil, local) != Verifier(local) {
		t.Error("NewChainVerifier(nil, local) is not the local verifier")
	}
	oidc := &stubVerifier{}
	if NewChainVerifier(oidc, nil) != Verifier(oidc) {
		t.Error("NewChainVerifier(oidc, nil) is not the OIDC verifier")
	}
	// The chain advertises the OIDC contract; local auth has no provider
	// endpoints to advertise.
	chain := NewChainVerifier(oidc, local)
	if chain.Config().Issuer != "https://stub.example.test" {
		t.Errorf("chain Config() = %+v, want the OIDC contract", chain.Config())
	}
}

func TestGenerateVerificationToken(t *testing.T) {
	raw1, hash1, err := GenerateVerificationToken()
	if err != nil {
		t.Fatalf("GenerateVerificationToken() error = %v", err)
	}
	raw2, hash2, err := GenerateVerificationToken()
	if err != nil {
		t.Fatalf("GenerateVerificationToken() error = %v", err)
	}
	if raw1 == raw2 || hash1 == hash2 {
		t.Error("two tokens collided")
	}
	if HashVerificationToken(raw1) != hash1 {
		t.Error("HashVerificationToken(raw) != stored hash")
	}
	if hash1 == raw1 {
		t.Error("hash equals the raw token; the raw token must never be stored")
	}
	if len(hash1) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(hash1))
	}
	if _, err := hex.DecodeString(hash1); err != nil {
		t.Errorf("hash is not hex: %v", err)
	}
	if strings.ContainsAny(raw1, "+/=") {
		t.Errorf("raw token %q is not URL-safe", raw1)
	}
}
