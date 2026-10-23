package form_test

import (
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/form"
)

func TestCheckSchemaAcceptsValid(t *testing.T) {
	schema := `{"type":"object","required":["email"],"properties":{"email":{"type":"string"}}}`
	if err := form.CheckSchema([]byte(schema)); err != nil {
		t.Errorf("CheckSchema() error = %v, want nil", err)
	}
}

func TestCheckFileFields(t *testing.T) {
	upload := func(files string) string {
		return `{"type":"object","required":["files"],"properties":{"files":` + files + `}}`
	}
	valid := upload(`{"type":"array","minItems":1,"items":{"type":"string","format":"binary"},` +
		`"allowed_mime_types":["application/pdf","image/*"],"max_file_bytes":1048576}`)
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"valid upload field", valid, ""},
		{"binary without constraints", upload(`{"type":"array","items":{"type":"string","format":"binary"}}`), ""},
		{"single binary field", `{"type":"object","properties":{"file":{"type":"string","format":"binary","max_file_bytes":10}}}`, ""},
		{"no properties", `{"type":"object"}`, ""},
		{"constraints on text ignored", `{"type":"object","properties":{"note":{"type":"string","max_file_bytes":-1}}}`, ""},
		{"bad mime shape", upload(`{"type":"array","items":{"type":"string","format":"binary"},"allowed_mime_types":["image"]}`), "allowed_mime_types"},
		{"double wildcard mime", upload(`{"type":"array","items":{"type":"string","format":"binary"},"allowed_mime_types":["*/*"]}`), "allowed_mime_types"},
		{"non-string mime", upload(`{"type":"array","items":{"type":"string","format":"binary"},"allowed_mime_types":[42]}`), "allowed_mime_types"},
		{"non-array mime", upload(`{"type":"array","items":{"type":"string","format":"binary"},"allowed_mime_types":"application/pdf"}`), "allowed_mime_types"},
		{"zero max bytes", upload(`{"type":"array","items":{"type":"string","format":"binary"},"max_file_bytes":0}`), "max_file_bytes"},
		{"negative max bytes", upload(`{"type":"array","items":{"type":"string","format":"binary"},"max_file_bytes":-1}`), "max_file_bytes"},
		{"string max bytes", upload(`{"type":"array","items":{"type":"string","format":"binary"},"max_file_bytes":"1MB"}`), "max_file_bytes"},
		{"fractional max bytes", upload(`{"type":"array","items":{"type":"string","format":"binary"},"max_file_bytes":1.5}`), "max_file_bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := form.CheckFileFields([]byte(c.raw))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckFileFields() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckFileFields() error = nil, want mention of %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("CheckFileFields() error = %q, want mention of %q", err, c.wantErr)
			}
		})
	}
}

func TestCheckSchemaRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", ``, "schema"},
		{"null", `null`, "schema"},
		{"non-object array", `[]`, "object"},
		{"non-object string", `"x"`, "object"},
		{"empty object", `{}`, "non-empty"},
		{"invalid JSON", `{`, "valid JSON"},
		{"uncompilable schema", `{"type":"object","properties":{"name":{"type":"string","pattern":"(["}}}`, "valid JSON Schema"},
	}
	for _, c := range cases {
		if err := form.CheckSchema([]byte(c.raw)); err == nil {
			t.Errorf("%s: CheckSchema() error = nil, want error", c.name)
			continue
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: CheckSchema() error = %q, want mention of %q", c.name, err, c.want)
		}
	}
}
