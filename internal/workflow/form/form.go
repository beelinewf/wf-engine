// Package form checks that a forms-library schema document is itself a valid
// JSON Schema before it is stored. Payloads are never validated against a
// form backend-side: input delivery is gated solely by the node's validation
// script.
package form

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// CheckSchema reports whether schema is a non-empty JSON object that compiles
// as a JSON Schema (draft 2020-12).
func CheckSchema(schema []byte) error {
	if len(schema) == 0 || strings.TrimSpace(string(schema)) == "" || string(schema) == "null" {
		return errors.New("schema is required")
	}
	var doc any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return fmt.Errorf("schema must be valid JSON: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return errors.New("schema must be an object")
	}
	if len(obj) == 0 {
		return errors.New("schema must be a non-empty object")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("form.json", doc); err != nil {
		return fmt.Errorf("schema is not a valid JSON Schema: %w", err)
	}
	if _, err := c.Compile("form.json"); err != nil {
		return fmt.Errorf("schema is not a valid JSON Schema: %w", err)
	}
	return nil
}
