package repository

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"gorm.io/gorm"
)

// Secret values are encrypted at rest with AES-256-GCM. Two stores carry
// them: the secrets table (one value per row) and the reserved "secret"
// root of every persisted workflow context (instance, branch, node
// attempt, and lean-history copies). Encryption lives only in this
// package: domain objects above the repository always hold plaintext, so
// templates, scripts, and the redaction views behave exactly as before.
//
// Stored values carry the secretCipherPrefix so rows written before
// encryption existed stay readable: a value without the prefix is legacy
// plaintext and passes through until its next write re-encrypts it. There
// is no key rotation: every replica must run with the same key, and
// losing the key makes the stored values unreadable.
const secretCipherPrefix = "enc:v1:"

// secretCipherState holds the cipher installed at startup. It is swapped
// atomically like the envsnapshot override: readers always load one
// consistent cipher. A nil state means no key was installed (tests and
// tooling that never call InstallSecretEncryption) and values pass
// through unchanged; the application binary validates that a key is
// configured and installs it before serving, so production never runs
// in pass-through mode.
var secretCipherState atomic.Pointer[secretCipher]

type secretCipher struct {
	aead cipher.AEAD
}

// InstallSecretEncryption installs the at-rest cipher from the configured
// base64-encoded 32-byte key. It is called once at startup; a malformed
// key fails the boot instead of silently storing plaintext.
func InstallSecretEncryption(encodedKey string) error {
	key, err := DecodeSecretEncryptionKey(encodedKey)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("secret encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("secret encryption: %w", err)
	}
	secretCipherState.Store(&secretCipher{aead: aead})
	return nil
}

// ClearSecretEncryption removes the installed cipher. It exists for tests
// that install a throwaway key; production never calls it.
func ClearSecretEncryption() {
	secretCipherState.Store(nil)
}

// secretVerifySample bounds the boot probe: only the most recent rows of
// each context store are checked, so startup cost never grows with the
// history tables.
const secretVerifySample = 25

// VerifySecretEncryptionReadable spot-checks that the installed key can
// open sealed secret values already at rest. The startup probe calls it
// after the secrets-table check: with a wrong key and an empty (or
// all-legacy) secrets table, that check alone would pass while stored
// instance, branch, attempt, and history contexts stay unreadable.
// Legacy plaintext rows carry no sealed values and pass trivially.
func VerifySecretEncryptionReadable(ctx context.Context, db *gorm.DB) error {
	var instances []WorkflowInstanceModel
	if err := db.WithContext(ctx).Model(&WorkflowInstanceModel{}).
		Select("context").Order(`"created_at" DESC`).Limit(secretVerifySample).
		Find(&instances).Error; err != nil {
		return fmt.Errorf("secret encryption verify: %w", err)
	}
	for _, row := range instances {
		if err := verifySealedContext(json.RawMessage(row.Context)); err != nil {
			return fmt.Errorf("secret encryption verify workflow_instances.context: %w", err)
		}
	}
	var branches []ParallelBranchModel
	if err := db.WithContext(ctx).Model(&ParallelBranchModel{}).
		Select("context").Order(`"created_at" DESC`).Limit(secretVerifySample).
		Find(&branches).Error; err != nil {
		return fmt.Errorf("secret encryption verify: %w", err)
	}
	for _, row := range branches {
		if err := verifySealedContext(json.RawMessage(row.Context)); err != nil {
			return fmt.Errorf("secret encryption verify parallel_branches.context: %w", err)
		}
	}
	var attempts []NodeInstanceModel
	if err := db.WithContext(ctx).Model(&NodeInstanceModel{}).
		Select("context_before", "context_after").Order(`"created_at" DESC`).Limit(secretVerifySample).
		Find(&attempts).Error; err != nil {
		return fmt.Errorf("secret encryption verify: %w", err)
	}
	for _, row := range attempts {
		if err := verifySealedContext(json.RawMessage(row.ContextBefore)); err != nil {
			return fmt.Errorf("secret encryption verify node_instances.context_before: %w", err)
		}
		if err := verifySealedContext(json.RawMessage(row.ContextAfter)); err != nil {
			return fmt.Errorf("secret encryption verify node_instances.context_after: %w", err)
		}
	}
	var history []NodeContextHistoryModel
	if err := db.WithContext(ctx).Model(&NodeContextHistoryModel{}).
		Select("snapshot", "diff").Order(`"created_at" DESC`).Limit(secretVerifySample).
		Find(&history).Error; err != nil {
		return fmt.Errorf("secret encryption verify: %w", err)
	}
	for _, row := range history {
		if err := verifySealedContext(json.RawMessage(row.Snapshot)); err != nil {
			return fmt.Errorf("secret encryption verify node_context_history.snapshot: %w", err)
		}
		if err := verifySealedContext(json.RawMessage(row.Diff)); err != nil {
			return fmt.Errorf("secret encryption verify node_context_history.diff: %w", err)
		}
	}
	return nil
}

// verifySealedContext strictly opens every sealed string in one stored
// payload (full context secret root, or secret entries of a lean diff).
func verifySealedContext(raw json.RawMessage) error {
	obj, ok := decodeContextForCipher(raw)
	if !ok {
		return nil
	}
	if root, ok := obj["secret"].(map[string]any); ok {
		if err := verifySealedLeaves(root); err != nil {
			return err
		}
	}
	if set, ok := obj["set"].(map[string]any); ok {
		for path, value := range set {
			if path != "secret" && !strings.HasPrefix(path, "secret.") {
				continue
			}
			if err := verifySealedLeaves(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifySealedLeaves(value any) error {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, secretCipherPrefix) {
			_, err := decryptSecretValue(typed)
			return err
		}
		return nil
	case map[string]any:
		for _, child := range typed {
			if err := verifySealedLeaves(child); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, child := range typed {
			if err := verifySealedLeaves(child); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// DecodeSecretEncryptionKey parses the configured key: base64 of exactly
// 32 bytes (AES-256). Configuration validation mirrors this rule at load
// time (it cannot import this package), so keep the two in sync.
func DecodeSecretEncryptionKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("secret encryption key: not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret encryption key: decoded length is %d bytes, want 32", len(key))
	}
	return key, nil
}

func loadSecretCipher() *secretCipher {
	return secretCipherState.Load()
}

// encryptSecretValue seals one plaintext value. Callers always pass
// domain plaintext (the repository boundary keeps stored bytes below
// it), so a value that merely starts with the format prefix is data and
// seals like any other. A missing cipher passes the value through
// unchanged. Sealing can only fail on a broken random source, which is
// unrecoverable for a crypto boundary, so it panics rather than risking
// a silent plaintext write.
func encryptSecretValue(plaintext string) string {
	c := loadSecretCipher()
	if c == nil {
		return plaintext
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(fmt.Sprintf("secret encryption: read nonce: %v", err))
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return secretCipherPrefix + base64.StdEncoding.EncodeToString(sealed)
}

var errSecretUndecryptable = errors.New("secret value cannot be decrypted with the configured key")

// decryptSecretValue opens one stored value. Legacy plaintext (no prefix)
// passes through. A sealed value that cannot be opened — wrong key or no
// key installed — is an error: callers that can fail (the secrets table)
// surface it instead of handing ciphertext to a template.
func decryptSecretValue(stored string) (string, error) {
	if !strings.HasPrefix(stored, secretCipherPrefix) {
		return stored, nil
	}
	c := loadSecretCipher()
	if c == nil {
		return "", errSecretUndecryptable
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, secretCipherPrefix))
	if err != nil {
		return "", errSecretUndecryptable
	}
	if len(sealed) < c.aead.NonceSize() {
		return "", errSecretUndecryptable
	}
	plain, err := c.aead.Open(nil, sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():], nil)
	if err != nil {
		return "", errSecretUndecryptable
	}
	return string(plain), nil
}

// decryptSecretValueLenient is decryptSecretValue for the mapping layer,
// whose signatures cannot fail. An undecryptable value is returned as
// stored: API views still mask it (it is just a string under the secret
// root), and the startup probe over the secrets table catches a wrong
// key before serving. Returning anything else would fabricate a secret.
func decryptSecretValueLenient(stored string) string {
	plain, err := decryptSecretValue(stored)
	if err != nil {
		return stored
	}
	return plain
}

// encryptContextSecrets seals every string under the "secret" root of a
// full workflow context. The JSON shape is unchanged (the column stays
// JSONB, key names stay readable); without a secret root, without string
// values, or without an installed cipher the input is returned untouched
// so stored bytes stay identical to what callers produced.
func encryptContextSecrets(raw json.RawMessage) json.RawMessage {
	return transformContextSecrets(raw, func(value string) string {
		return encryptSecretValue(value)
	})
}

// decryptContextSecrets reverses encryptContextSecrets for a full context.
func decryptContextSecrets(raw json.RawMessage) json.RawMessage {
	return transformContextSecrets(raw, decryptSecretValueLenient)
}

func transformContextSecrets(raw json.RawMessage, transform func(string) string) json.RawMessage {
	if loadSecretCipher() == nil {
		return raw
	}
	obj, ok := decodeContextForCipher(raw)
	if !ok {
		return raw
	}
	secretRoot, ok := obj["secret"].(map[string]any)
	if !ok {
		return raw
	}
	// The snapshot is flat by construction, but a script can replace the
	// root with nested shapes; seal every string leaf below it. Scalars
	// keep their type: they are not values the secrets table issued.
	changed := false
	for key, value := range secretRoot {
		if next, ch := transformSecretLeaves(value, transform); ch {
			secretRoot[key] = next
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return marshalContextForCipher(obj, raw)
}

// transformSecretLeaves rewrites every string in a decoded JSON value
// and reports whether anything changed. Maps and slices are rebuilt in
// place; scalars keep their type and value.
func transformSecretLeaves(value any, transform func(string) string) (any, bool) {
	switch typed := value.(type) {
	case string:
		next := transform(typed)
		return next, next != typed
	case map[string]any:
		changed := false
		for key, child := range typed {
			if next, ch := transformSecretLeaves(child, transform); ch {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, child := range typed {
			if next, ch := transformSecretLeaves(child, transform); ch {
				typed[i] = next
				changed = true
			}
		}
		return typed, changed
	default:
		return value, false
	}
}

// encryptDiffSecrets seals secret values inside a lean-context diff: the
// "set" entries addressed secret.<key>. The whole-root form ("secret"
// itself carrying an object) is sealed per value as well.
func encryptDiffSecrets(raw json.RawMessage) json.RawMessage {
	return transformDiffSecrets(raw, func(value string) string {
		return encryptSecretValue(value)
	})
}

// decryptDiffSecrets reverses encryptDiffSecrets.
func decryptDiffSecrets(raw json.RawMessage) json.RawMessage {
	return transformDiffSecrets(raw, decryptSecretValueLenient)
}

func transformDiffSecrets(raw json.RawMessage, transform func(string) string) json.RawMessage {
	if loadSecretCipher() == nil {
		return raw
	}
	obj, ok := decodeContextForCipher(raw)
	if !ok {
		return raw
	}
	set, ok := obj["set"].(map[string]any)
	if !ok {
		return raw
	}
	changed := false
	for path, value := range set {
		if path != "secret" && !strings.HasPrefix(path, "secret.") {
			continue
		}
		// A "secret.<key>" entry is one value; a bare "secret" entry is a
		// whole-root replacement whose string leaves seal recursively.
		if next, ch := transformSecretLeaves(value, transform); ch {
			set[path] = next
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return marshalContextForCipher(obj, raw)
}

// encryptContextSecretsAuto seals a payload that is either a full context
// (node attempt context_before) or a lean diff (context_after in lean
// mode). The shapes are told apart by their keys: a diff carries set and
// unset, a full context is sealed when it carries a secret root.
func encryptContextSecretsAuto(raw json.RawMessage) json.RawMessage {
	if isDiffShaped(raw) {
		return encryptDiffSecrets(raw)
	}
	return encryptContextSecrets(raw)
}

// decryptContextSecretsAuto reverses encryptContextSecretsAuto.
func decryptContextSecretsAuto(raw json.RawMessage) json.RawMessage {
	if isDiffShaped(raw) {
		return decryptDiffSecrets(raw)
	}
	return decryptContextSecrets(raw)
}

func isDiffShaped(raw json.RawMessage) bool {
	obj, ok := decodeContextForCipher(raw)
	if !ok {
		return false
	}
	if _, hasSecret := obj["secret"]; hasSecret {
		return false
	}
	_, hasSet := obj["set"]
	_, hasUnset := obj["unset"]
	return hasSet && hasUnset
}

// contextHasSealedSecrets reports whether a (decrypted) context still
// carries sealed secret values, i.e. the installed key could not open
// them. Callers that scrub by value must fail closed in that case:
// matching plaintext against ciphertext would silently leak.
func contextHasSealedSecrets(raw json.RawMessage) bool {
	obj, ok := decodeContextForCipher(raw)
	if !ok {
		return false
	}
	secretRoot, ok := obj["secret"].(map[string]any)
	if !ok {
		return false
	}
	found := false
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			if strings.HasPrefix(typed, secretCipherPrefix) {
				found = true
			}
		case map[string]any:
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(secretRoot)
	return found
}

func decodeContextForCipher(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

func marshalContextForCipher(obj map[string]any, fallback json.RawMessage) json.RawMessage {
	out, err := json.Marshal(obj)
	if err != nil {
		return fallback
	}
	return out
}
