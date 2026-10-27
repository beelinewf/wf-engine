package service

import (
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

func testContentGraph() *contentGraph {
	inner := &model.NodeContent{ID: "inner", Type: model.NodeTypeScript}
	group := &model.NodeContent{
		ID:   "g1",
		Type: model.NodeTypeGroup,
		Group: &model.GroupContent{
			StartNodeID: "inner",
			Nodes:       []*model.NodeContent{inner},
		},
	}
	top := &model.NodeContent{ID: "top", Type: model.NodeTypeScript}
	return &contentGraph{wc: &model.WorkflowContent{
		StartNodeID: "top",
		Nodes:       []*model.NodeContent{top, group},
	}}
}

func TestContentGraphFindNode(t *testing.T) {
	g := testContentGraph()
	for _, id := range []string{"top", "g1", "inner"} {
		n, err := g.FindNode(id)
		if err != nil {
			t.Fatalf("FindNode(%q) error = %v", id, err)
		}
		if n.ID != id {
			t.Errorf("FindNode(%q).ID = %q", id, n.ID)
		}
	}
	if _, err := g.FindNode("missing"); err == nil {
		t.Error("FindNode(missing) error = nil, want unknown-node error")
	}
}

func TestContentGraphGroupStack(t *testing.T) {
	g := testContentGraph()
	stack, err := g.GroupStack("inner")
	if err != nil {
		t.Fatalf("GroupStack(inner) error = %v", err)
	}
	if len(stack) != 1 || stack[0] != "g1" {
		t.Errorf("GroupStack(inner) = %v, want [g1]", stack)
	}
	stack, err = g.GroupStack("top")
	if err != nil {
		t.Fatalf("GroupStack(top) error = %v", err)
	}
	if len(stack) != 0 {
		t.Errorf("GroupStack(top) = %v, want empty", stack)
	}
	if _, err := g.GroupStack("missing"); err == nil {
		t.Error("GroupStack(missing) error = nil, want unknown-node error")
	}
}
