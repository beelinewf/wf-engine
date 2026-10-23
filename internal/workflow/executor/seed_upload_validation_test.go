package executor_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/form"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// seedUploadValidationScript loads the input_upload node's validation script
// from scripts/seed.sh, so this test guards the shipped seed contract rather
// than a copy of it.
func seedUploadValidationScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "seed.sh"))
	if err != nil {
		t.Fatalf("read seed.sh: %v", err)
	}
	const start = "node_definitions=$(cat <<'JSON'\n"
	i := strings.Index(string(raw), start)
	if i < 0 {
		t.Fatal("seed.sh: node_definitions heredoc not found")
	}
	rest := string(raw)[i+len(start):]
	end := strings.Index(rest, "\nJSON\n")
	if end < 0 {
		t.Fatal("seed.sh: node_definitions heredoc end not found")
	}
	var defs map[string]struct {
		Content struct {
			Validation struct {
				Script string `json:"script"`
			} `json:"validation"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(rest[:end]), &defs); err != nil {
		t.Fatalf("parse node_definitions: %v", err)
	}
	script := defs["input_upload"].Content.Validation.Script
	if script == "" {
		t.Fatal("seed.sh: input_upload validation script is empty")
	}
	return script
}

// seedUploadForm loads the upload-form payload from scripts/seed.sh, so
// this test guards the shipped form definition rather than a copy of it.
func seedUploadForm(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "seed.sh"))
	if err != nil {
		t.Fatalf("read seed.sh: %v", err)
	}
	const start = "upload_form_payload=$(cat <<'JSON'\n"
	i := strings.Index(string(raw), start)
	if i < 0 {
		t.Fatal("seed.sh: upload_form_payload heredoc not found")
	}
	rest := string(raw)[i+len(start):]
	end := strings.Index(rest, "\nJSON\n")
	if end < 0 {
		t.Fatal("seed.sh: upload_form_payload heredoc end not found")
	}
	var form map[string]any
	if err := json.Unmarshal([]byte(rest[:end]), &form); err != nil {
		t.Fatalf("parse upload_form_payload: %v", err)
	}
	return form
}

// The upload form declares what the user provides: files is a file
// upload and fields is text. It must not describe the server-generated
// file references ({original_file_name, file_name, size, mime_type})
// that a multipart delivery writes into context.
func TestSeedUploadFormDeclaresFileAndTextFields(t *testing.T) {
	doc := seedUploadForm(t)
	if got := doc["content_type"]; got != "multipart/form-data" {
		t.Errorf("upload-form content_type = %v, want multipart/form-data", got)
	}
	schema, ok := doc["schema"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: schema missing")
	}
	rawSchema, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal upload-form schema: %v", err)
	}
	if err := form.CheckSchema(rawSchema); err != nil {
		t.Errorf("upload-form schema does not compile: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: schema.properties missing")
	}
	if _, ok := props["field"]; ok {
		t.Error("upload-form: schema still declares singular \"field\", want \"fields\"")
	}
	files, ok := props["files"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: schema.properties.files missing")
	}
	if got := files["type"]; got != "array" {
		t.Errorf("files.type = %v, want \"array\"", got)
	}
	items, ok := files["items"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: files.items missing")
	}
	if got := items["type"]; got != "string" {
		t.Errorf("files.items.type = %v, want \"string\" (upload, not a JSON object)", got)
	}
	if got := items["format"]; got != "binary" {
		t.Errorf("files.items.format = %v, want \"binary\"", got)
	}
	if _, ok := items["properties"]; ok {
		t.Error("files.items still declares object properties (file-reference shape), want binary upload")
	}
	// File constraints live on the form's files field itself, so the
	// frontend can validate size and MIME type from the form alone.
	if got := files["max_file_bytes"]; got != float64(1048576) {
		t.Errorf("files.max_file_bytes = %v, want 1048576", got)
	}
	allowed, ok := files["allowed_mime_types"].([]any)
	if !ok || len(allowed) != 1 || allowed[0] != "application/pdf" {
		t.Errorf("files.allowed_mime_types = %v, want [application/pdf]", files["allowed_mime_types"])
	}
	fields, ok := props["fields"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: schema.properties.fields missing")
	}
	if got := fields["type"]; got != "array" {
		t.Errorf("fields.type = %v, want \"array\"", got)
	}
	fitems, ok := fields["items"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: fields.items missing")
	}
	if got := fitems["type"]; got != "string" {
		t.Errorf("fields.items.type = %v, want \"string\"", got)
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("upload-form: schema.required missing")
	}
	want := map[string]bool{"files": false, "fields": false}
	for _, r := range required {
		if s, ok := r.(string); ok {
			if _, tracked := want[s]; tracked {
				want[s] = true
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("schema.required missing %q", name)
		}
	}
}

// seedUploadNode loads the input_upload node definition content from
// scripts/seed.sh, so the form/node parity test guards the shipped seed
// rather than a copy of it.
func seedUploadNode(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "seed.sh"))
	if err != nil {
		t.Fatalf("read seed.sh: %v", err)
	}
	const start = "node_definitions=$(cat <<'JSON'\n"
	i := strings.Index(string(raw), start)
	if i < 0 {
		t.Fatal("seed.sh: node_definitions heredoc not found")
	}
	rest := string(raw)[i+len(start):]
	end := strings.Index(rest, "\nJSON\n")
	if end < 0 {
		t.Fatal("seed.sh: node_definitions heredoc end not found")
	}
	var defs map[string]struct {
		Content map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(rest[:end]), &defs); err != nil {
		t.Fatalf("parse node_definitions: %v", err)
	}
	content := defs["input_upload"].Content
	if len(content) == 0 {
		t.Fatal("seed.sh: input_upload content is empty")
	}
	return content
}

// The seed upload form and its input_upload node definition are one
// consistent block: the node's storage mirrors the form's file
// constraints, and the node's accept carries the form's content_type.
func TestSeedUploadFormNodeParity(t *testing.T) {
	form := seedUploadForm(t)
	node := seedUploadNode(t)

	formFiles, ok := form["schema"].(map[string]any)["properties"].(map[string]any)["files"].(map[string]any)
	if !ok {
		t.Fatal("upload-form: schema.properties.files missing")
	}
	storage, ok := node["storage"].(map[string]any)
	if !ok {
		t.Fatal("input_upload: storage missing")
	}
	formMIME, _ := json.Marshal(formFiles["allowed_mime_types"])
	nodeMIME, _ := json.Marshal(storage["allowed_mime_types"])
	if string(formMIME) != string(nodeMIME) {
		t.Errorf("mime allowlist differs: form %s, node %s", formMIME, nodeMIME)
	}
	if formFiles["max_file_bytes"] != storage["max_file_bytes"] {
		t.Errorf("max_file_bytes differs: form %v, node %v", formFiles["max_file_bytes"], storage["max_file_bytes"])
	}
	accept, ok := node["accept"].([]any)
	if !ok {
		t.Fatal("input_upload: accept missing")
	}
	want, _ := form["content_type"].(string)
	found := false
	for _, a := range accept {
		if s, ok := a.(string); ok && s == want {
			found = true
		}
	}
	if !found {
		t.Errorf("node accept %v does not carry the form content_type %q", accept, want)
	}
}

func TestSeedUploadValidationRequiresFilesAndFields(t *testing.T) {
	script := seedUploadValidationScript(t)
	ex := executor.NewExecutors(executor.Limits{}, nil, executor.Dependencies{})[model.NodeTypeInput]
	file := `{"original_file_name":"a.pdf","file_name":"x.pdf","size":10,"mime_type":"application/pdf"}`
	cases := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"accepts files and fields", `{"files":[` + file + `],"fields":["hello"]}`, true},
		{"rejects missing files", `{"fields":["hello"]}`, false},
		{"rejects missing fields", `{"files":[` + file + `]}`, false},
		{"rejects empty files", `{"files":[],"fields":["hello"]}`, false},
		{"rejects empty fields", `{"files":[` + file + `],"fields":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ex.Execute(context.Background(), executor.Request{
				Node:    inputNode(script),
				Context: map[string]any{},
				Payload: []byte(tc.payload),
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			vr := res.Output.(*executor.ValidationResult)
			if vr.Valid != tc.valid {
				t.Errorf("Valid = %v, want %v (message %q)", vr.Valid, tc.valid, vr.Message)
			}
			if !tc.valid && vr.Message == "" {
				t.Error("rejection carries no message")
			}
		})
	}
}
