package model

import "encoding/json"

// NodeTypeInfo is one entry of the node type catalog: the type name,
// whether it is a builtin, and its full node-object JSON Schema. A custom
// type registered without a schema carries a nil Schema; the read degrades
// to null rather than dropping the type.
type NodeTypeInfo struct {
	Type    string
	Builtin bool
	Schema  json.RawMessage
}

// NodeTypeCatalog lists every node type the engine knows: the builtins in
// declaration order, then the registered custom types in name order. The
// returned schemas are shared and must not be modified.
func NodeTypeCatalog() []NodeTypeInfo {
	names := builtinTypeNames()
	customs := CustomTypes()
	out := make([]NodeTypeInfo, 0, len(names)+len(customs))
	for _, t := range names {
		schema, _ := NodeSchema(t)
		out = append(out, NodeTypeInfo{Type: t, Builtin: true, Schema: schema})
	}
	for _, t := range customs {
		schema, _ := NodeSchema(t)
		out = append(out, NodeTypeInfo{Type: t, Schema: schema})
	}
	return out
}
