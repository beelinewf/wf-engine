package repository

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// testSecretKey is the base64 encoding of bytes 0..31. Test-only.
const testSecretKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func installTestCipher(t *testing.T) {
	t.Helper()
	if err := InstallSecretEncryption(testSecretKey); err != nil {
		t.Fatalf("InstallSecretEncryption() error = %v", err)
	}
	t.Cleanup(ClearSecretEncryption)
}

func TestSecretValueCipherRoundTrip(t *testing.T) {
	installTestCipher(t)

	stored := encryptSecretValue("plain-value")
	if stored == "plain-value" || strings.Contains(stored, "plain-value") {
		t.Fatalf("encryptSecretValue() = %q, want ciphertext without plaintext", stored)
	}
	if !strings.HasPrefix(stored, secretCipherPrefix) {
		t.Fatalf("encryptSecretValue() = %q, want %q prefix", stored, secretCipherPrefix)
	}
	// Random nonce: the same plaintext encrypts differently every time.
	if again := encryptSecretValue("plain-value"); again == stored {
		t.Fatal("encryptSecretValue() produced identical ciphertext twice, want random nonce")
	}
	got, err := decryptSecretValue(stored)
	if err != nil || got != "plain-value" {
		t.Fatalf("decryptSecretValue() = %q, %v; want plain-value", got, err)
	}
	// Callers pass domain plaintext only, so sealing is not idempotent
	// by design: a second seal would be a boundary bug, not a re-save.
}

func TestSecretValueLegacyPlaintextPassthrough(t *testing.T) {
	installTestCipher(t)

	// Rows written before encryption existed carry no prefix and stay
	// readable until their next write.
	got, err := decryptSecretValue("legacy-plain")
	if err != nil || got != "legacy-plain" {
		t.Fatalf("decryptSecretValue(legacy) = %q, %v; want legacy-plain", got, err)
	}
}

func TestInstallSecretEncryptionRejectsBadKeys(t *testing.T) {
	for _, key := range []string{"", "not-base64!!", "aGk="} {
		if err := InstallSecretEncryption(key); err == nil {
			t.Fatalf("InstallSecretEncryption(%q) error = nil, want error", key)
		}
	}
}

func TestContextSecretEncryptionRoundTrip(t *testing.T) {
	installTestCipher(t)

	raw := json.RawMessage(`{"order":{"total":1},"secret":{"API_KEY":"plain-value","TOKEN":"other"}}`)
	stored := encryptContextSecrets(raw)
	if strings.Contains(string(stored), "plain-value") || strings.Contains(string(stored), `"other"`) {
		t.Fatalf("encryptContextSecrets() = %s, want no plaintext secret values", stored)
	}
	// Non-secret content and the secret key names stay readable.
	var storedObj map[string]any
	if err := json.Unmarshal(stored, &storedObj); err != nil {
		t.Fatalf("stored context is not JSON: %v", err)
	}
	if _, ok := storedObj["secret"].(map[string]any)["API_KEY"]; !ok {
		t.Fatalf("stored context = %s, want secret key names preserved", stored)
	}
	back := decryptContextSecrets(stored)
	var got, want map[string]any
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("decrypt(encrypt(context)) = %s, want %s", gotJSON, wantJSON)
	}
	// Legacy plaintext contexts decrypt to themselves.
	if legacy := decryptContextSecrets(raw); string(legacy) != string(raw) {
		t.Fatalf("decryptContextSecrets(legacy) = %s, want unchanged", legacy)
	}
}

func TestDiffSecretEncryptionRoundTrip(t *testing.T) {
	installTestCipher(t)

	raw := json.RawMessage(`{"set":{"order.total":2,"secret.API_KEY":"plain-value"},"unset":["old"]}`)
	stored := encryptDiffSecrets(raw)
	if strings.Contains(string(stored), "plain-value") {
		t.Fatalf("encryptDiffSecrets() = %s, want no plaintext secret values", stored)
	}
	back := decryptDiffSecrets(stored)
	if string(back) != string(raw) {
		t.Fatalf("decryptDiffSecrets(encrypt) = %s, want %s", back, raw)
	}
	// The auto form (node context_after carries a full context or a diff)
	// routes both shapes.
	if auto := decryptContextSecretsAuto(encryptContextSecretsAuto(raw)); string(auto) != string(raw) {
		t.Fatalf("auto diff round trip = %s, want %s", auto, raw)
	}
	full := json.RawMessage(`{"secret":{"API_KEY":"plain-value"}}`)
	if auto := decryptContextSecretsAuto(encryptContextSecretsAuto(full)); string(auto) != string(full) {
		t.Fatalf("auto full round trip = %s, want %s", auto, full)
	}
}

func TestContextSecretEncryptionSealsNestedValues(t *testing.T) {
	installTestCipher(t)

	// A script can replace the secret root with nested shapes; every
	// string leaf under the root is sealed, scalars keep their type.
	raw := json.RawMessage(`{"secret":{"creds":{"password":"nested-plain","pins":[1234,"arr-plain"]},"API_KEY":"plain-value"}}`)
	stored := encryptContextSecrets(raw)
	if strings.Contains(string(stored), "nested-plain") || strings.Contains(string(stored), "arr-plain") {
		t.Fatalf("encryptContextSecrets() = %s, want nested strings sealed", stored)
	}
	back := decryptContextSecrets(stored)
	var got, want map[string]any
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("nested round trip = %s, want %s", gotJSON, wantJSON)
	}
}

func TestSecretValueWithCipherPrefixRoundTrips(t *testing.T) {
	installTestCipher(t)

	// A user-chosen value that happens to start with the format prefix
	// is data, not ciphertext: it must seal and reopen like any other.
	const tricky = "enc:v1:not-really-ciphertext"
	stored := encryptSecretValue(tricky)
	if stored == tricky {
		t.Fatal("encryptSecretValue() left a prefix-shaped plaintext unsealed")
	}
	got, err := decryptSecretValue(stored)
	if err != nil || got != tricky {
		t.Fatalf("decryptSecretValue() = %q, %v; want %q", got, err, tricky)
	}
}

func TestScheduleAndRequestMappersEncryptContextAtRest(t *testing.T) {
	installTestCipher(t)

	schedule := CronScheduleToModel(model.CronSchedule{
		ID:      "11111111-1111-7111-8111-111111111111",
		Context: json.RawMessage(`{"secret":{"API_KEY":"plain-value"}}`),
	})
	if strings.Contains(string(schedule.Context), "plain-value") {
		t.Fatalf("CronScheduleToModel().Context = %s, want no plaintext", schedule.Context)
	}
	if back := CronScheduleFromModel(schedule); string(back.Context) != `{"secret":{"API_KEY":"plain-value"}}` {
		t.Fatalf("schedule mapper round trip = %s", back.Context)
	}

	request := WorkflowRequestToModel(model.WorkflowRequest{
		ID:      "11111111-1111-7111-8111-111111111111",
		Context: json.RawMessage(`{"secret":{"API_KEY":"plain-value"}}`),
	})
	if strings.Contains(string(request.Context), "plain-value") {
		t.Fatalf("WorkflowRequestToModel().Context = %s, want no plaintext", request.Context)
	}
	if back := WorkflowRequestFromModel(request); string(back.Context) != `{"secret":{"API_KEY":"plain-value"}}` {
		t.Fatalf("request mapper round trip = %s", back.Context)
	}
}

func TestRedactStatusUpdateErrorFailsClosedWhenUndecryptable(t *testing.T) {
	installTestCipher(t)
	stored := encryptContextSecrets(json.RawMessage(`{"secret":{"API_KEY":"plain-value"}}`))
	// Simulate a key that cannot open the stored values: scrubbing must
	// mask the whole error instead of leaking it unscrubbed.
	ClearSecretEncryption()
	if got := redactStatusUpdateError(stored, "request failed with plain-value"); got != statusUpdateSecretMask {
		t.Fatalf("redactStatusUpdateError() = %q, want %q", got, statusUpdateSecretMask)
	}
}

func TestNoCipherLeavesContextsUntouched(t *testing.T) {
	ClearSecretEncryption()

	raw := json.RawMessage(`{"secret":{"API_KEY":"plain-value"}}`)
	if got := encryptContextSecrets(raw); string(got) != string(raw) {
		t.Fatalf("encryptContextSecrets() without cipher = %s, want unchanged", got)
	}
	if got := encryptSecretValue("plain-value"); got != "plain-value" {
		t.Fatalf("encryptSecretValue() without cipher = %q, want unchanged", got)
	}
}

func TestInstanceMapperEncryptsContextAtRest(t *testing.T) {
	installTestCipher(t)

	inst := model.WorkflowInstance{
		ID:      "11111111-1111-7111-8111-111111111111",
		Context: json.RawMessage(`{"secret":{"API_KEY":"plain-value"},"run_id":"r1"}`),
	}
	row := WorkflowInstanceToModel(inst)
	if strings.Contains(string(row.Context), "plain-value") {
		t.Fatalf("WorkflowInstanceToModel().Context = %s, want no plaintext", row.Context)
	}
	back := WorkflowInstanceFromModel(row)
	// JSON object key order is not part of the context contract (the
	// JSONB column normalizes it too), so compare decoded values.
	var gotCtx, wantCtx map[string]any
	if err := json.Unmarshal(back.Context, &gotCtx); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(inst.Context, &wantCtx); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(gotCtx)
	wantJSON, _ := json.Marshal(wantCtx)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("mapper round trip = %s, want %s", gotJSON, wantJSON)
	}
}
