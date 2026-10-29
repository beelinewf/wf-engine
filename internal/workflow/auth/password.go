package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is the work factor for local password hashes. Twelve keeps a
// login under a few hundred milliseconds on commodity hardware while
// staying expensive to crack offline.
const BcryptCost = 12

// dummyPasswordHash is a valid cost-12 bcrypt hash of a fixed secret, used
// only so unknown-email logins cost the same as real comparisons. It
// authenticates nothing: DummyPasswordCompare always reports false.
const dummyPasswordHash = "$2a$12$DYqyBWRTTQsycmQ0HF.mS.TJt0V7mJJHyWqefZOzrTBiI4EeQT.HC"

// HashPassword hashes a plaintext password for storage. It fails closed on
// an empty password and on anything bcrypt refuses (notably passwords past
// its 72-byte limit); the service validates length first so the error here
// is defense in depth rather than the user-facing check.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: password is empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches the stored hash. Every
// error (mismatch, malformed hash, empty input) is false: there is exactly
// one thing a caller may do with a password check, and it needs no reason.
func CheckPassword(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// DummyPasswordCompare burns one cost-12 comparison and reports false. A
// login for an unknown email calls it instead of CheckPassword, so the
// timing of "no such user" matches "wrong password" and the address cannot
// be probed by latency.
func DummyPasswordCompare(password string) bool {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
	return false
}
