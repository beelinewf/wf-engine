package model_test

import (
	"sort"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// catalogByType indexes a catalog by type name, failing on duplicates.
func catalogByType(t *testing.T, catalog []model.NodeTypeInfo) map[string]model.NodeTypeInfo {
	t.Helper()
	out := make(map[string]model.NodeTypeInfo, len(catalog))
	for _, entry := range catalog {
		if _, dup := out[entry.Type]; dup {
			t.Fatalf("catalog lists %q twice", entry.Type)
		}
		out[entry.Type] = entry
	}
	return out
}

func TestNodeTypeCatalogListsBuiltinsFirst(t *testing.T) {
	catalog := model.NodeTypeCatalog()
	if len(catalog) < len(builtinTypes) {
		t.Fatalf("catalog has %d entries, want at least %d", len(catalog), len(builtinTypes))
	}
	for i, want := range builtinTypes {
		got := catalog[i]
		if got.Type != want || !got.Builtin {
			t.Errorf("catalog[%d] = %+v, want type %q flagged builtin", i, got, want)
		}
		if len(got.Schema) == 0 {
			t.Errorf("catalog[%d] (%s) has an empty schema", i, want)
		}
	}
}

// TestNodeTypeCatalogInputFormIDDocumented pins the input type's distinctive
// field: the catalog documents the forms-library reference, while the form
// bodies live in the forms library, not in node content.
func TestNodeTypeCatalogInputFormIDDocumented(t *testing.T) {
	entry, ok := catalogByType(t, model.NodeTypeCatalog())["input"]
	if !ok {
		t.Fatal("catalog missing the input type")
	}
	props, _ := decodeSchema(t, entry.Schema)["properties"].(map[string]any)
	if _, ok := props["form_id"]; !ok {
		t.Error("input schema documents no form_id property")
	}
	if _, legacy := props["form"]; legacy {
		t.Error("input schema still advertises the legacy form object")
	}
}

func TestNodeTypeCatalogIncludesCustomTypes(t *testing.T) {
	const typ = "catalogtest1"
	registerSchemaType(t, typ, validConfigSchema)
	catalog := model.NodeTypeCatalog()
	entry, ok := catalogByType(t, catalog)[typ]
	if !ok {
		t.Fatalf("catalog missing custom type %q", typ)
	}
	if entry.Builtin {
		t.Errorf("custom type %q flagged builtin", typ)
	}
	if len(entry.Schema) == 0 {
		t.Errorf("custom type %q has an empty schema", typ)
	}
	// Customs follow the builtins in name order.
	tail := catalog[len(builtinTypes):]
	if !sort.SliceIsSorted(tail, func(i, j int) bool { return tail[i].Type < tail[j].Type }) {
		t.Errorf("custom tail is not sorted: %v", tail)
	}
}

// TestNodeTypeCatalogCustomWithoutSchemaIsNull covers the degraded read: a
// validator-only registration is listed with no schema rather than dropped.
func TestNodeTypeCatalogCustomWithoutSchemaIsNull(t *testing.T) {
	const typ = "catalogtest_noschema"
	if err := model.RegisterCustomType(typ, testValidator); err != nil {
		t.Fatalf("RegisterCustomType() error = %v", err)
	}
	t.Cleanup(func() { model.UnregisterCustomType(typ) })
	entry, ok := catalogByType(t, model.NodeTypeCatalog())[typ]
	if !ok {
		t.Fatalf("catalog missing custom type %q", typ)
	}
	if len(entry.Schema) != 0 {
		t.Errorf("schema-less custom type carries %d schema bytes, want none", len(entry.Schema))
	}
}
