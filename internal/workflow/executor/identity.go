package executor

// Identity carries the execution identity of one node run: the workflow
// instance, the node occurrence, and the parallel scope (empty outside a
// branch). It is the single source for the reserved template roots and
// the frozen script "meta" global, so templates and scripts can never
// disagree. The zero value is the parent scope with unknown node.
type Identity struct {
	WorkflowInstanceID  string
	NodeInstanceID      string
	BranchID            string
	ParallelExecutionID string
	BranchName          string
	BranchIndex         int
	NodeID              string
}

// Meta returns the read-only script view of the identity. All keys are
// always present; branch keys are empty outside a branch scope.
func (i Identity) Meta() map[string]any {
	return map[string]any{
		"workflow_instance_id":  i.WorkflowInstanceID,
		"node_instance_id":      i.NodeInstanceID,
		"branch_id":             i.BranchID,
		"parallel_execution_id": i.ParallelExecutionID,
		"branch_name":           i.BranchName,
		"branch_index":          i.BranchIndex,
		"node_id":               i.NodeID,
	}
}

// TemplateData returns a shallow copy of ctx enriched with the reserved
// automatic template roots, which always win over same-named user values.
// The result is only for rendering; it is never written back to the
// workflow context and the input map is not mutated.
func (i Identity) TemplateData(ctx map[string]any) map[string]any {
	data := make(map[string]any, len(ctx)+7)
	for k, v := range ctx {
		data[k] = v
	}
	for k, v := range i.Meta() {
		data[k] = v
	}
	return data
}
