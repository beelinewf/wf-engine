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
