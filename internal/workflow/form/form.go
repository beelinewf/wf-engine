// Package form checks that a forms-library schema document is itself a valid
// JSON Schema before it is stored. Payloads are never validated against a
// form backend-side: input delivery is gated solely by the node's validation
// script.
package form

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

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

// CheckFileFields validates the file-upload conventions of a schema that
// already passed CheckSchema: a property declaring format binary (directly
// or under items) may carry allowed_mime_types and max_file_bytes, and
// those must be well-formed when present. Non-binary properties are
// ignored; the two keys are annotations the compiler does not check.
func CheckFileFields(schema []byte) error {
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return fmt.Errorf("schema must be valid JSON: %w", err)
	}
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		return nil
	}
	for name, raw := range props {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !isBinaryField(prop) {
			continue
		}
		if err := checkMIMETypes(name, prop); err != nil {
			return err
		}
		if err := checkMaxFileBytes(name, prop); err != nil {
			return err
		}
	}
	return nil
}

func isBinaryField(prop map[string]any) bool {
	if prop["format"] == "binary" {
		return true
	}
	items, ok := prop["items"].(map[string]any)
	return ok && items["format"] == "binary"
}

func checkMIMETypes(name string, prop map[string]any) error {
	raw, ok := prop["allowed_mime_types"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("property %q: allowed_mime_types must be an array", name)
	}
	for i, entry := range list {
		s, ok := entry.(string)
		if !ok {
			return fmt.Errorf("property %q: allowed_mime_types[%d] must be a string", name, i)
		}
		if !validMIMEType(s) {
			return fmt.Errorf("property %q: allowed_mime_types[%d] %q must be \"type/subtype\" or \"type/*\"", name, i, s)
		}
	}
	return nil
}

// validMIMEType mirrors the node storage allowlist rule: type/subtype or
// type/*, lowercased, no whitespace, no bare or double wildcard.
func validMIMEType(entry string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	mediaType, subtype, ok := strings.Cut(entry, "/")
	if !ok || mediaType == "" || subtype == "" || strings.Contains(subtype, "/") ||
		strings.Contains(mediaType, "*") || subtype != "*" && strings.Contains(subtype, "*") ||
		strings.IndexFunc(entry, unicode.IsSpace) >= 0 {
		return false
	}
	return true
}

func checkMaxFileBytes(name string, prop map[string]any) error {
	raw, ok := prop["max_file_bytes"]
	if !ok {
		return nil
	}
	n, ok := raw.(float64)
	if !ok || n != math.Trunc(n) || n <= 0 {
		return fmt.Errorf("property %q: max_file_bytes must be a positive integer", name)
	}
	return nil
}
