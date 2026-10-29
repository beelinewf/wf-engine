package auth_test

import (
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if hash == "" || hash == "correct horse battery staple" {
		t.Fatalf("HashPassword() returned %q, want a hash", hash)
	}
	if !auth.CheckPassword(hash, "correct horse battery staple") {
		t.Error("CheckPassword(match) = false, want true")
	}
	if auth.CheckPassword(hash, "wrong password") {
		t.Error("CheckPassword(mismatch) = true, want false")
	}
}

func TestCheckPasswordFailsClosed(t *testing.T) {
	hash, err := auth.HashPassword("secret")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	for name, pair := range map[string][2]string{
		"empty hash":     {"", "secret"},
		"empty password": {hash, ""},
		"malformed hash": {"not-a-hash", "secret"},
	} {
		if auth.CheckPassword(pair[0], pair[1]) {
			t.Errorf("%s: CheckPassword = true, want false", name)
		}
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := auth.HashPassword(""); err == nil {
		t.Error("HashPassword(\"\") error = nil, want an error")
	}
}

// bcrypt refuses passwords past 72 bytes; the service validates length
// first, and this pins the defense in depth behind it.
func TestHashPasswordRejectsTooLong(t *testing.T) {
	if _, err := auth.HashPassword(strings.Repeat("x", 73)); err == nil {
		t.Error("HashPassword(73 bytes) error = nil, want an error")
	}
}

// The dummy comparison authenticates nothing: it only burns the same work
// as a real check so unknown-email logins time identically.
func TestDummyPasswordCompareAlwaysFalse(t *testing.T) {
	if auth.DummyPasswordCompare("anything") {
		t.Error("DummyPasswordCompare() = true, want false")
	}
}
