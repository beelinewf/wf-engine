package executor_test

import (
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

func testIdentity() executor.Identity {
	return executor.Identity{
		WorkflowInstanceID:  "inst-1",
		NodeInstanceID:      "occ-1",
		BranchID:            "br-1",
		ParallelExecutionID: "ex-1",
		BranchName:          "billing",
		BranchIndex:         2,
		NodeID:              "notify",
	}
}

func TestIdentityMetaKeys(t *testing.T) {
	m := testIdentity().Meta()
	want := map[string]any{
		"workflow_instance_id":  "inst-1",
		"node_instance_id":      "occ-1",
		"branch_id":             "br-1",
		"parallel_execution_id": "ex-1",
		"branch_name":           "billing",
		"branch_index":          2,
		"node_id":               "notify",
	}
	if len(m) != len(want) {
		t.Fatalf("Meta() has %d keys, want %d: %v", len(m), len(want), m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Meta()[%q] = %v, want %v", k, m[k], v)
		}
	}
}

func TestIdentityMetaParentScopeHasAllKeys(t *testing.T) {
	m := executor.Identity{}.Meta()
	for _, k := range []string{"workflow_instance_id", "node_instance_id", "branch_id", "parallel_execution_id", "branch_name", "branch_index", "node_id"} {
		if _, ok := m[k]; !ok {
			t.Errorf("zero Identity.Meta() missing key %q", k)
		}
	}
	if m["branch_index"] != 0 {
		t.Errorf("zero branch_index = %v, want 0", m["branch_index"])
	}
}

func TestTemplateDataReservedWinsAndNoMutation(t *testing.T) {
	ctx := map[string]any{"branch_id": "user-value", "keep": "yes"}
	data := testIdentity().TemplateData(ctx)
	if data["branch_id"] != "br-1" {
		t.Errorf("branch_id = %v, want reserved value", data["branch_id"])
	}
	if data["keep"] != "yes" {
		t.Errorf("keep = %v, want yes", data["keep"])
	}
	if ctx["branch_id"] != "user-value" {
		t.Errorf("input context mutated: %v", ctx)
	}
	if _, ok := ctx["node_id"]; ok {
		t.Errorf("input context gained reserved key: %v", ctx)
	}
}

func TestRequestIdentityAndTemplateData(t *testing.T) {
	req := executor.Request{
		Node:                &model.NodeContent{ID: "notify"},
		Context:             map[string]any{"a": 1},
		InstanceID:          "inst-1",
		NodeInstanceID:      "occ-1",
		BranchID:            "br-1",
		ParallelExecutionID: "ex-1",
		BranchName:          "billing",
		BranchIndex:         2,
	}
	ident := req.Identity()
	if ident != testIdentity() {
		t.Errorf("Identity() = %+v, want %+v", ident, testIdentity())
	}
	data := req.TemplateData()
	if data["branch_id"] != "br-1" || data["node_id"] != "notify" || data["a"] != 1 {
		t.Errorf("TemplateData() = %v", data)
	}
}

func TestRequestIdentityNilNode(t *testing.T) {
	req := executor.Request{InstanceID: "inst-1"}
	if got := req.Identity().WorkflowInstanceID; got != "inst-1" {
		t.Errorf("WorkflowInstanceID = %q", got)
	}
	if got := req.Identity().NodeID; got != "" {
		t.Errorf("NodeID = %q, want empty", got)
	}
}
