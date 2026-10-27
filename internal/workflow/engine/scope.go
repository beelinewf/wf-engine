package engine

import (
	"encoding/json"

	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// execScope is the execution unit of one transition: a workflow instance
// (parent scope) or one parallel branch. Transition methods operate on a
// scope so node execution, failure routing, and recovery stay shared; only
// state commits and terminal mappings differ per kind.
// scopeKind names which execution unit a scope covers. The constructors
// set it; isBranch reads it instead of inferring from branchID.
type scopeKind int

const (
	scopeParent scopeKind = iota
	scopeBranch
)

type execScope struct {
	kind       scopeKind
	instanceID string
	// branchID is "" for the parent scope.
	branchID string
	// executionID and endNodeID join a branch scope to its barrier.
	executionID string
	endNodeID   string
	// branchName, branchIndex, and branchStartNodeID identify the branch
	// in lifecycle events. Empty for the parent scope.
	branchName        string
	branchIndex       int
	branchStartNodeID string
	// depth is the nesting level of the execution the scope runs in: 0
	// for the parent scope, the execution depth for a branch scope. A
	// fork from this scope creates depth+1.
	depth int

	contextMode string
	leasedBy    string
	revision    int64

	// fromStatus/fromReason feed the status-update outbox on parent commits.
	fromStatus model.WorkflowStatus
	fromReason model.WaitingReason

	pauseRequested bool
	debug          bool

	contextRaw  json.RawMessage
	frameRaw    json.RawMessage
	countersRaw json.RawMessage

	definitionID string
}

func (s *execScope) isBranch() bool { return s.kind == scopeBranch }

// branchIdentity returns the stable branch identifiers for lifecycle
// events. appendEvent adds branch_id and parallel_execution_id.
func (s *execScope) branchIdentity() map[string]any {
	return map[string]any{
		"branch_name":   s.branchName,
		"branch_index":  s.branchIndex,
		"start_node_id": s.branchStartNodeID,
		"end_node_id":   s.endNodeID,
	}
}

// identity builds the executor identity for a node run in this scope.
// nodeInstanceID is "" where no attempt exists yet (group entry, input
// parking, joins).
func (s *execScope) identity(nodeID, nodeInstanceID string) executor.Identity {
	return executor.Identity{
		WorkflowInstanceID:  s.instanceID,
		NodeInstanceID:      nodeInstanceID,
		BranchID:            s.branchID,
		ParallelExecutionID: s.executionID,
		BranchName:          s.branchName,
		BranchIndex:         s.branchIndex,
		NodeID:              nodeID,
	}
}

// idempotencyKey uniquely identifies one logical step for outbound calls.
// The parent format is unchanged; branch steps add the branch id.
func (s *execScope) idempotencyKey(attemptID string) string {
	if s.isBranch() {
		return s.instanceID + ":" + s.branchID + ":" + attemptID
	}
	return s.instanceID + ":" + attemptID
}

// instanceScope builds the parent scope from a reloaded instance row.
func instanceScope(w model.WorkflowInstance) *execScope {
	return &execScope{
		kind:           scopeParent,
		instanceID:     w.ID,
		definitionID:   w.WorkflowDefinitionID,
		contextMode:    w.ContextMode,
		leasedBy:       w.LeasedBy,
		revision:       w.Revision,
		fromStatus:     w.Status,
		fromReason:     w.WaitingReason,
		pauseRequested: w.PauseRequested,
		debug:          w.Debug,
		contextRaw:     w.Context,
		frameRaw:       w.Frame,
		countersRaw:    w.Counters,
	}
}

// branchScope builds a branch scope from reloaded branch and execution rows.
// Branches always run full-context semantics: their rows carry complete
// snapshots and never append lean history, so parent replay stays clean.
// Debug comes from the instance row: the branch step-through parks each
// branch transition paused.
func branchScope(b model.ParallelBranch, ex model.ParallelExecution, debug bool) *execScope {
	return &execScope{
		kind:              scopeBranch,
		instanceID:        b.InstanceID,
		branchID:          b.ID,
		executionID:       b.ParallelExecutionID,
		endNodeID:         ex.EndNodeID,
		branchName:        b.Name,
		branchIndex:       b.BranchIndex,
		branchStartNodeID: b.StartNodeID,
		depth:             ex.Depth,
		contextMode:       "full",
		leasedBy:          b.LeasedBy,
		revision:          b.Revision,
		debug:             debug,
		contextRaw:        b.Context,
		frameRaw:          b.Frame,
		countersRaw:       b.Counters,
	}
}
