package repository_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/repository"
)

// testEncryptionKey is the base64 encoding of bytes 0..31. Test-only.
const testEncryptionKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func installEncryption(t *testing.T) {
	t.Helper()
	if err := repository.InstallSecretEncryption(testEncryptionKey); err != nil {
		t.Fatalf("InstallSecretEncryption() error = %v", err)
	}
	t.Cleanup(repository.ClearSecretEncryption)
}

// The F1 acceptance check: with a key installed, no plaintext secret value
// appears in the secrets table or in a persisted instance context, while
// every domain read still returns the plaintext.
func TestSecretEncryptionAtRest(t *testing.T) {
	db := setupTestDB(t)
	installEncryption(t)
	ctx := context.Background()

	secrets := repository.NewSecretRepository(db)
	if _, err := secrets.Create(ctx, "API_KEY", "plain-value"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	var storedValue string
	if err := db.Raw(`SELECT value FROM secrets WHERE key = ?`, "API_KEY").Scan(&storedValue).Error; err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if strings.Contains(storedValue, "plain-value") || !strings.HasPrefix(storedValue, "enc:v1:") {
		t.Fatalf("stored secret = %q, want enc:v1: ciphertext without plaintext", storedValue)
	}
	all, err := secrets.GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll() error = %v", err)
	}
	if all["API_KEY"] != "plain-value" {
		t.Fatalf("GetAll()[API_KEY] = %q, want plain-value", all["API_KEY"])
	}

	seedInstanceFixture(t, db)
	inst := newTestInstance("11111111-1111-7111-8111-111111111111",
		"waiting", "runnable")
	inst.Context = json.RawMessage(`{"secret":{"API_KEY":"plain-value"},"run_id":"r1"}`)
	insertInstance(t, db, inst)

	var storedContext string
	if err := db.Raw(`SELECT context::text FROM workflow_instances WHERE id = ?`, inst.ID).Scan(&storedContext).Error; err != nil {
		t.Fatalf("read stored context: %v", err)
	}
	if strings.Contains(storedContext, "plain-value") {
		t.Fatalf("stored context = %s, want no plaintext secret value", storedContext)
	}
	if !strings.Contains(storedContext, "enc:v1:") || !strings.Contains(storedContext, "API_KEY") {
		t.Fatalf("stored context = %s, want sealed value under the visible key name", storedContext)
	}

	got, err := repository.NewInstanceRepository(db).GetByID(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	var gotObj map[string]any
	if err := json.Unmarshal(got.Context, &gotObj); err != nil {
		t.Fatal(err)
	}
	if gotObj["secret"].(map[string]any)["API_KEY"] != "plain-value" {
		t.Fatalf("GetByID() context = %s, want decrypted secret root", got.Context)
	}
}

// A wrong key must be detectable at boot even when the secrets table
// itself is empty: sealed values in stored contexts are spot-checked.
func TestVerifySecretEncryptionReadable(t *testing.T) {
	db := setupTestDB(t)
	installEncryption(t)
	ctx := context.Background()

	seedInstanceFixture(t, db)
	inst := newTestInstance("11111111-1111-7111-8111-111111111111",
		"waiting", "runnable")
	inst.Context = json.RawMessage(`{"secret":{"API_KEY":"plain-value"}}`)
	insertInstance(t, db, inst)

	if err := repository.VerifySecretEncryptionReadable(ctx, db); err != nil {
		t.Fatalf("VerifySecretEncryptionReadable() with correct key = %v, want nil", err)
	}

	// A different valid key (base64 of bytes 32..63) cannot open the
	// stored values and must be reported.
	if err := repository.InstallSecretEncryption("ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="); err != nil {
		t.Fatalf("InstallSecretEncryption(other) error = %v", err)
	}
	if err := repository.VerifySecretEncryptionReadable(ctx, db); err == nil {
		t.Fatal("VerifySecretEncryptionReadable() with wrong key = nil, want error")
	}
}

// Rows written before encryption existed carry no prefix: they stay
// readable, and their next write seals them.
func TestSecretEncryptionLegacyPlaintextReadable(t *testing.T) {
	db := setupTestDB(t)
	installEncryption(t)
	ctx := context.Background()

	if err := db.Exec(`INSERT INTO secrets (key, value, created_at, updated_at) VALUES (?, ?, now(), now())`,
		"LEGACY", "legacy-plain").Error; err != nil {
		t.Fatalf("insert legacy secret: %v", err)
	}
	secrets := repository.NewSecretRepository(db)
	all, err := secrets.GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll() error = %v", err)
	}
	if all["LEGACY"] != "legacy-plain" {
		t.Fatalf("GetAll()[LEGACY] = %q, want legacy-plain", all["LEGACY"])
	}
}
