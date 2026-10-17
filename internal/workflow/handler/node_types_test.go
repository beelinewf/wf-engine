package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/simpwf/workflow-engine/internal/workflow/model"
)

func TestNodeTypesList(t *testing.T) {
	svc := &fakeNodeSvc{types: []model.NodeTypeInfo{
		{Type: "script", Builtin: true, Schema: json.RawMessage(`{"title":"script"}`)},
		{Type: "custom_widget", Schema: json.RawMessage(`{"title":"widget"}`)},
	}}
	r := nodeRouter(svc)
	w := performJSON(r, http.MethodGet, "/v1/node/types", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			Type    string          `json:"type"`
			Builtin bool            `json:"builtin"`
			Schema  json.RawMessage `json:"schema"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Type != "script" || !resp.Items[0].Builtin {
		t.Errorf("items[0] = %+v, want script flagged builtin", resp.Items[0])
	}
	if resp.Items[1].Type != "custom_widget" || resp.Items[1].Builtin {
		t.Errorf("items[1] = %+v, want custom_widget not flagged builtin", resp.Items[1])
	}
	for i, want := range []string{`{"title":"script"}`, `{"title":"widget"}`} {
		if string(resp.Items[i].Schema) != want {
			t.Errorf("items[%d].schema = %s, want %s", i, resp.Items[i].Schema, want)
		}
	}
}

// TestNodeTypesListEmptyIsArray: with no types the envelope carries [] and
// not null, so a frontend can iterate it without a nil check.
func TestNodeTypesListEmptyIsArray(t *testing.T) {
	r := nodeRouter(&fakeNodeSvc{})
	w := performJSON(r, http.MethodGet, "/v1/node/types", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Items == nil {
		t.Errorf("items = null, want [] (body: %s)", w.Body.String())
	}
}
