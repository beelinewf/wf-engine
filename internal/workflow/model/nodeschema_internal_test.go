package model

import "testing"

func TestSetGroupChildRefMissingProperties(t *testing.T) {
	if err := setGroupChildRef(map[string]any{}); err == nil {
		t.Error("setGroupChildRef(empty) error = nil, want descriptive error")
	}
}

func TestSetGroupChildRefMissingNodes(t *testing.T) {
	doc := map[string]any{"properties": map[string]any{}}
	if err := setGroupChildRef(doc); err == nil {
		t.Error("setGroupChildRef(no nodes) error = nil, want descriptive error")
	}
}

func TestSetGroupChildRefSetsItems(t *testing.T) {
	nodes := map[string]any{}
	doc := map[string]any{"properties": map[string]any{"nodes": nodes}}
	if err := setGroupChildRef(doc); err != nil {
		t.Fatalf("setGroupChildRef() error = %v", err)
	}
	items, ok := nodes["items"].(map[string]any)
	if !ok || items["$ref"] != "#/$defs/childNode" {
		t.Errorf("nodes.items = %v, want childNode ref", nodes["items"])
	}
}
